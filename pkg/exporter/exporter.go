// Copyright © 2020 Bin Liu <bin.liu@enmotech.com>

package exporter

import (
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"net/url"
	"strings"
	"time"
)

type Exporter struct {
	dsn                    []string
	configPath             string   // config file path /directory
	disableCache           bool     // always execute query when been scrapped
	autoDiscovery          bool     // discovery other database on primary server
	failFast               bool     // fail fast instead fof waiting during start-up ?
	excludedDatabases      []string // excluded database for auto discovery
	disableSettingsMetrics bool
	tags                   []string
	namespace              string
	servers                *Servers
	metricMap              map[string]*Query

	constantLabels   prometheus.Labels
	duration         prometheus.Gauge
	error            prometheus.Gauge
	up               prometheus.Gauge
	userQueriesError *prometheus.GaugeVec
	totalScrapes     prometheus.Counter
}

func NewExporter(opts ...Opt) (e *Exporter, err error) {
	e = &Exporter{
		metricMap: defaultMonList,
	}
	for _, opt := range opts {
		opt(e)
	}
	if err := e.loadConfig(); err != nil {
		return nil, err
	}
	e.setupInternalMetrics()
	e.setupServers()
	return e, nil
}
func (e *Exporter) loadConfig() error {
	if e.configPath == "" {
		for _, q := range e.metricMap {
			_ = q.Check()
		}
		return nil
	}
	queryList, err := LoadConfig(e.configPath)
	if err != nil {
		return err
	}
	for name, query := range queryList {
		var found bool
		for defName, defQuery := range e.metricMap {
			if strings.EqualFold(defQuery.Name, query.Name) {
				e.metricMap[defName] = query
				found = true
				break
			}
		}
		if !found {
			e.metricMap[name] = query
		}
	}
	return nil
}

func (e *Exporter) GetConfigList() map[string]*Query {
	if e.metricMap == nil {
		return nil
	}
	return e.metricMap
}
func (e *Exporter) setupInternalMetrics() {

	e.duration = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   e.namespace,
		Subsystem:   "exporter",
		Name:        "last_scrape_duration_seconds",
		Help:        "Duration of the last scrape of metrics from OpenGauss.",
		ConstLabels: e.constantLabels,
	})
	e.totalScrapes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace:   e.namespace,
		Subsystem:   "exporter",
		Name:        "scrapes_total",
		Help:        "Total number of times OpenGauss was scraped for metrics.",
		ConstLabels: e.constantLabels,
	})
	e.error = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   e.namespace,
		Subsystem:   "exporter",
		Name:        "last_scrape_error",
		Help:        "Whether the last scrape of metrics from OpenGauss resulted in an error (1 for error, 0 for success).",
		ConstLabels: e.constantLabels,
	})
	e.up = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace:   e.namespace,
		Name:        "up",
		Help:        "Whether the last scrape of metrics from OpenGauss was able to connect to the server (1 for yes, 0 for no).",
		ConstLabels: e.constantLabels,
	})
	e.userQueriesError = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace:   e.namespace,
		Subsystem:   "exporter",
		Name:        "user_queries_load_error",
		Help:        "Whether the user queries file was loaded and parsed successfully (1 for error, 0 for success).",
		ConstLabels: e.constantLabels,
	}, []string{"filename", "hashsum"})
}

func (e *Exporter) setupServers() {
	e.servers = NewServers(ServerWithLabels(e.constantLabels),
		ServerWithNamespace(e.namespace),
		ServerWithDisableSettingsMetrics(e.disableSettingsMetrics),
		ServerWithDisableCache(e.disableCache),
	)
}

// Describe implement prometheus.Collector

// -> Collect
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	metricCh := make(chan prometheus.Metric)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	e.Collect(metricCh)
	close(metricCh)
	<-doneCh
}

// Collect
// Collect->
// 		scrape->
//			-> discoverDatabaseDSNs
//			-> scrapeDSN
//				-> GetServer
// 				-> checkMapVersions
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	ch <- e.up
	e.userQueriesError.Collect(ch)
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
	}(time.Now())

	e.totalScrapes.Inc()

	dsnList := e.dsn
	if e.autoDiscovery {
		dsnList = e.discoverDatabaseDSNs()
	}

	var errorsCount int
	var connectionErrorsCount int

	for _, dsn := range dsnList {
		if err := e.scrapeDSN(ch, dsn); err != nil {
			errorsCount++

			log.Errorf(err.Error())

			if _, ok := err.(*ErrorConnectToServer); ok {
				connectionErrorsCount++
			}
		}
	}

	switch {
	case connectionErrorsCount >= len(dsnList):
		e.up.Set(0)
	default:
		e.up.Set(1) // Didn't fail, can mark connection as up for this scrape.
	}

	switch errorsCount {
	case 0:
		e.error.Set(0)
	default:
		e.error.Set(1)
	}
}

func (e *Exporter) discoverDatabaseDSNs() []string {
	dsnList := make(map[string]struct{})
	for _, dsn := range e.dsn {
		parsedDSN, err := url.Parse(dsn)
		if err != nil {
			log.Errorf("Unable to parse DSN (%s): %v", ShadowDSN(dsn), err)
			continue
		}

		dsnList[dsn] = struct{}{}
		server, err := e.servers.GetServer(dsn)
		if err != nil {
			log.Errorf("Error opening connection to database (%s): %v", ShadowDSN(dsn), err)
			continue
		}

		// If autoDiscoverDatabases is true, set first dsn as master database (Default: false)
		server.master = true

		databaseNames, err := server.QueryDatabases()
		if err != nil {
			log.Errorf("Error querying databases (%s): %v", ShadowDSN(dsn), err)
			continue
		}
		for _, databaseName := range databaseNames {
			if Contains(e.excludedDatabases, databaseName) {
				continue
			}
			parsedDSN.Path = databaseName
			dsnList[parsedDSN.String()] = struct{}{}
		}
	}

	result := make([]string, len(dsnList))
	index := 0
	for dsn := range dsnList {
		result[index] = dsn
		index++
	}

	return result
}

func (e *Exporter) scrapeDSN(ch chan<- prometheus.Metric, dsn string) error {
	server, err := e.servers.GetServer(dsn)

	if err != nil {
		return &ErrorConnectToServer{fmt.Sprintf("Error opening connection to database (%s): %s", ShadowDSN(dsn), err.Error())}
	}

	// Check if autoDiscoverDatabases is false, set dsn as master database (Default: false)
	if !e.autoDiscovery {
		server.master = true
	}

	// Check if map versions need to be updated
	if err := e.checkMapVersions(ch, server); err != nil {
		log.Warnln("Proceeding with outdated query maps, as the Postgres version could not be determined:", err)
	}

	return server.Scrape(ch, false)
}

func (e *Exporter) checkMapVersions(ch chan<- prometheus.Metric, server *Server) error {
	log.Debugf("Querying OpenGauss Version on %q", server)
	versionRow := server.db.QueryRow("SELECT version();")
	var versionString string
	err := versionRow.Scan(&versionString)
	if err != nil {
		return fmt.Errorf("Error scanning version string on %q: %v ", server, err)
	}
	semanticVersion, err := parseVersionSem(versionString)
	if err != nil {
		return fmt.Errorf("Error parsing version string on %q: %v ", server, err)
	}
	// Check if semantic version changed and recalculate maps if needed.
	if semanticVersion.NE(server.lastMapVersion) || server.metricMap == nil {
		log.Infof("Semantic Version Changed on %q: %s -> %s", server, server.lastMapVersion, semanticVersion)
		server.mappingMtx.Lock()
		server.metricMap = e.metricMap
		server.lastMapVersion = semanticVersion
		server.mappingMtx.Unlock()

	}

	versionDesc := prometheus.NewDesc(fmt.Sprintf("%s_%s", e.namespace, staticLabelName),
		"Version string as reported by postgres", []string{"version", "short_version"}, server.labels)

	if server.master {
		ch <- prometheus.MustNewConstMetric(versionDesc,
			prometheus.UntypedValue, 1, parseVersion(versionString), semanticVersion.String())
	}
	return nil
}

func (e *Exporter) Check() error {
	return nil
}

func (e *Exporter) Close() {
	e.servers.Close()
}