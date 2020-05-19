package main

import (
	"encoding/json"
	"fmt"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/mitchellh/mapstructure"
	"github.com/namsral/flag"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	namespace        = "powerdns"
	apiInfoEndpoint  = "servers/localhost"
	apiStatsEndpoint = "servers/localhost/statistics"
)

var (
	httpClient *retryablehttp.Client

	listenAddress = flag.String("listen_address", ":9120",
		"Address to listen on for web interface and telemetry.")
	metricsPath   = flag.String("metric_path", "/metrics",
		"Path under which to expose metrics.")
	apiURL        = flag.String("api_url", "http://localhost:8001/",
		"Base-URL of PowerDNS authoritative server/recursor API.")
	apiKey        = flag.String("api_key", "",
		"PowerDNS API Key")
)

// ServerInfo is used to parse JSON data from 'server/localhost' endpoint
type ServerInfo struct {
	Kind       string `mapstructure:"kind"`
	ID         string `mapstructure:"id"`
	URL        string `mapstructure:"url"`
	DaemonType string `mapstructure:"daemon_type"`
	Version    string `mapstructure:"version"`
	ConfigUrl  string `mapstructure:"config_url"`
	ZonesUrl   string `mapstructure:"zones_url"`
}

// StatsEntry is used to parse JSON data from 'server/localhost/statistics' endpoint
type StatsEntry struct {
	Name  string  `json:"name"`
	Kind  string  `json:"type"`
	Value string `json:"value"`
}

// Exporter collects PowerDNS stats from the given HostURL and exports them using
// the prometheus metrics package.
type Exporter struct {
	HostURL    *url.URL
	ServerType string
	ApiKey     string
	mutex      sync.RWMutex

	up                prometheus.Gauge
	totalScrapes      prometheus.Counter
	jsonParseFailures prometheus.Counter
	gaugeMetrics      map[int]prometheus.Gauge
	counterVecMetrics map[int]*prometheus.CounterVec
	gaugeDefs         []gaugeDefinition
	counterVecDefs    []counterVecDefinition
	client            *http.Client
}

func newCounterVecMetric(serverType, metricName, docString string, labelNames []string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: serverType,
			Name:      metricName,
			Help:      docString,
		},
		labelNames,
	)
}

func newGaugeMetric(serverType, metricName, docString string) prometheus.Gauge {
	return prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: serverType,
			Name:      metricName,
			Help:      docString,
		},
	)
}

// NewExporter returns an initialized Exporter.
func NewExporter(apiKey, serverType string, hostURL *url.URL) *Exporter {
	var gaugeDefs []gaugeDefinition
	var counterVecDefs []counterVecDefinition

	gaugeMetrics := make(map[int]prometheus.Gauge)
	counterVecMetrics := make(map[int]*prometheus.CounterVec)

	switch serverType {
	case "recursor":
		gaugeDefs = recursorGaugeDefs
		counterVecDefs = recursorCounterVecDefs
	case "authoritative":
		gaugeDefs = authoritativeGaugeDefs
		counterVecDefs = authoritativeCounterVecDefs
	case "dnsdist":
		gaugeDefs = dnsdistGaugeDefs
		counterVecDefs = dnsdistCounterVecDefs
	}

	for _, def := range gaugeDefs {
		gaugeMetrics[def.id] = newGaugeMetric(serverType, def.name, def.desc)
	}

	for _, def := range counterVecDefs {
		counterVecMetrics[def.id] = newCounterVecMetric(serverType, def.name, def.desc, []string{def.label})
	}

	return &Exporter{
		HostURL:    hostURL,
		ServerType: serverType,
		ApiKey:     apiKey,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: serverType,
			Name:      "up",
			Help:      "Was the last scrape of PowerDNS successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: serverType,
			Name:      "exporter_total_scrapes",
			Help:      "Current total PowerDNS scrapes.",
		}),
		jsonParseFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: serverType,
			Name:      "exporter_json_parse_failures",
			Help:      "Number of errors while parsing PowerDNS JSON stats.",
		}),
		gaugeMetrics:      gaugeMetrics,
		counterVecMetrics: counterVecMetrics,
		gaugeDefs:         gaugeDefs,
		counterVecDefs:    counterVecDefs,
	}
}

// Describe describes all the metrics ever exported by the PowerDNS exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range e.counterVecMetrics {
		m.Describe(ch)
	}
	for _, m := range e.gaugeMetrics {
		ch <- m.Desc()
	}
	ch <- e.up.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.jsonParseFailures.Desc()
}

// Collect fetches the stats from configured PowerDNS API URI and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	jsonStats := make(chan []StatsEntry)

	go e.scrape(jsonStats)

	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.resetMetrics()
	statsMap := e.setMetrics(jsonStats)
	ch <- e.up
	ch <- e.totalScrapes
	ch <- e.jsonParseFailures
	e.collectMetrics(ch, statsMap)
}

func (e *Exporter) scrape(jsonStats chan<- []StatsEntry) {
	defer close(jsonStats)

	e.totalScrapes.Inc()

	scrapeURL := newURL(e.HostURL, apiStatsEndpoint)
	data, err := getJSON(scrapeURL, e.ApiKey)
	if err != nil {
		e.up.Set(0)
		e.jsonParseFailures.Inc()
		log.Errorf("Error scraping PowerDNS: %v", err)
		return
	}

	var statsEntry []StatsEntry
	err = mapstructure.Decode(data, &statsEntry)

	e.up.Set(1)

	jsonStats <- statsEntry
}

func (e *Exporter) resetMetrics() {
	for _, m := range e.counterVecMetrics {
		m.Reset()
	}
}

func (e *Exporter) collectMetrics(ch chan<- prometheus.Metric, statsMap map[string]float64) {
	for _, m := range e.counterVecMetrics {
		m.Collect(ch)
	}
	for _, m := range e.gaugeMetrics {
		ch <- m
	}

	if e.ServerType == "recursor" {
		h, err := makeRecursorRTimeHistogram(statsMap)
		if err != nil {
			log.Errorf("Could not create response time histogram: %v", err)
			return
		}
		ch <- h
	}
}

func (e *Exporter) setMetrics(jsonStats <-chan []StatsEntry) (statsMap map[string]float64) {
	statsMap = make(map[string]float64)
	stats := <-jsonStats
	for _, s := range stats {
		value, err := strconv.ParseFloat(s.Value, 64)
		if err != nil {
			continue
			//log.Error(fmt.Sprintf("Unable to parse float: %+v", s))
		}
		statsMap[s.Name] = value
	}
	if len(statsMap) == 0 {
		return
	}

	for _, def := range e.gaugeDefs {
		if value, ok := statsMap[def.key]; ok {
			// latency gauges need to be converted from microseconds to seconds
			if strings.HasSuffix(def.key, "latency") {
				value = value / 1000000
			}
			e.gaugeMetrics[def.id].Set(value)
		} else {
			log.Errorf("Expected PowerDNS stats key not found: %s", def.key)
			e.jsonParseFailures.Inc()
		}
	}

	for _, def := range e.counterVecDefs {
		for key, label := range def.labelMap {
			if value, ok := statsMap[key]; ok {
				e.counterVecMetrics[def.id].WithLabelValues(label).Add(value)
			} else {
				log.Errorf("Expected PowerDNS stats key not found: %s", key)
				e.jsonParseFailures.Inc()
			}
		}
	}
	return
}

func getServerInfo(hostURL *url.URL, apiKey string) (info ServerInfo, err error) {
	serverInfoURL := newURL(hostURL, apiInfoEndpoint)
	data, err := getJSON(serverInfoURL, apiKey)
	if err != nil {
		return
	}

	err = mapstructure.Decode(data, &info)

	return
}

func getJSON(url, apiKey string) (data interface{}, err error) {
	req, err := retryablehttp.NewRequest("GET", url, nil)
	if err != nil {
		return
	}

	req.Header.Add("X-API-Key", apiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var content []byte
		content, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return
		}

		err = fmt.Errorf(string(content))
		return
	}

	if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return
	}

	return
}

func newURL(hostURL *url.URL, path string) string {
	endpointURI, _ := url.Parse(path)
	u := hostURL.ResolveReference(endpointURI)
	return u.String()
}

func main() {
	var ()
	flag.Parse()

	hostURL, err := url.Parse(*apiURL)
	if err != nil {
		log.Fatalf("Error parsing api-url: %v", err)
	}

	httpClient = retryablehttp.NewClient()

	server, err := getServerInfo(hostURL, *apiKey)
	if err != nil {
		log.Fatalf("Could not fetch PowerDNS server info: %v", err)
	}

	exporter := NewExporter(*apiKey, server.DaemonType, hostURL)
	prometheus.MustRegister(exporter)

	log.Infof("Starting Server: %s", *listenAddress)
	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>PowerDNS Exporter</title></head>
             <body>
             <h1>PowerDNS Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
