package main

import (
	"database/sql"
	"flag"
	_ "github.com/go-sql-driver/mysql"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"
)

const (
	namespace = "mysql"
)

var (
	listenAddress = flag.String("web.listen-address", ":9104", "Address to listen on for web interface and telemetry.")
	metricPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
)

type Exporter struct {
	dsn               string
	mutex             sync.RWMutex
	duration, error   prometheus.Gauge
	totalScrapes      prometheus.Counter
	metrics           map[string]prometheus.Gauge
	commands          *prometheus.CounterVec
	connectionErrors  *prometheus.CounterVec
	innodbRows        *prometheus.CounterVec
	performanceSchema *prometheus.CounterVec
}

// return new empty exporter
func NewMySQLExporter(dsn string) *Exporter {
	return &Exporter{
		dsn: dsn,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "exporter_last_scrape_duration_seconds",
			Help:      "The last scrape duration.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_scrapes_total",
			Help:      "Current total mysqld scrapes.",
		}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "exporter_last_scrape_error",
			Help:      "The last scrape error status.",
		}),
		metrics: map[string]prometheus.Gauge{},
		commands: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "commands_total",
			Help:      "Number of executed mysql commands.",
		}, []string{"command"}),
		connectionErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "connection_errors_total",
			Help:      "Number of mysql connection errors.",
		}, []string{"error"}),
		innodbRows: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "innodb_rows_total",
			Help:      "Mysql Innodb row operations.",
		}, []string{"operation"}),
		performanceSchema: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "performance_schema_total",
			Help:      "Mysql instrumentations that could not be loaded or created due to memory constraints",
		}, []string{"instrumentation"}),
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range e.metrics {
		m.Describe(ch)
	}

	e.commands.Describe(ch)
	e.connectionErrors.Describe(ch)
	e.innodbRows.Describe(ch)
	e.performanceSchema.Describe(ch)

	ch <- e.duration.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.error.Desc()
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	scrapes := make(chan []string)

	go e.scrape(scrapes)

	e.mutex.Lock()
	defer e.mutex.Unlock()
	e.setMetrics(scrapes)
	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	e.collectMetrics(ch)
	e.commands.Collect(ch)
	e.connectionErrors.Collect(ch)
	e.innodbRows.Collect(ch)
	e.performanceSchema.Collect(ch)
}

func (e *Exporter) scrape(scrapes chan<- []string) {
	defer close(scrapes)

	now := time.Now().UnixNano()

	e.totalScrapes.Inc()

	db, err := sql.Open("mysql", e.dsn)
	if err != nil {
		log.Println("error opening connection to database:", err)
		e.error.Set(1)
		e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
		return
	}
	defer db.Close()

	// fetch database status
	rows, err := db.Query("SHOW GLOBAL STATUS")
	if err != nil {
		log.Println("error running status query on database:", err)
		e.error.Set(1)
		e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
		return
	}
	defer rows.Close()

	var key, val []byte

	for rows.Next() {
		// get RawBytes from data
		err = rows.Scan(&key, &val)
		if err != nil {
			log.Println("error getting result set:", err)
			return
		}

		var res []string = make([]string, 2)
		res[0] = string(key)
		res[1] = string(val)

		scrapes <- res
	}

	// fetch slave status
	rows, err = db.Query("SHOW SLAVE STATUS")
	if err != nil {
		log.Println("error running show slave query on database:", err)
		e.error.Set(1)
		e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
		return
	}
	defer rows.Close()

	var slaveCols []string

	slaveCols, err = rows.Columns()
	if err != nil {
		log.Println("error retrieving column list:", err)
		e.error.Set(1)
		e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
		return
	}

	var slaveData = make([]sql.RawBytes, len(slaveCols))

	// As the number of columns varies with mysqld versions,
	// and sql.Scan requires []interface{}, we need to create a
	// slice of pointers to the elements of slaveData.

	scanArgs := make([]interface{}, len(slaveCols))
	for i := range slaveData {
		scanArgs[i] = &slaveData[i]
	}

	for rows.Next() {

		err = rows.Scan(scanArgs...)
		if err != nil {
			log.Println("error retrieving result set:", err)
			e.error.Set(1)
			e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
			return
		}

	}

	for i, col := range slaveCols {

		var res []string = make([]string, 2)

		res[0] = col
		res[1] = parseStatus(slaveData[i])

		scrapes <- res

	}

	e.error.Set(0)
	e.duration.Set(float64(time.Now().UnixNano()-now) / 1000000000)
}

func (e *Exporter) setGenericMetric(name string, value float64) {
	if _, ok := e.metrics[name]; !ok {
		e.metrics[name] = prometheus.NewUntyped(prometheus.UntypedOpts{
			Namespace: namespace,
			Name:      name,
		})
	}

	e.metrics[name].Set(value)
}

func (e *Exporter) setMetrics(scrapes <-chan []string) {
	var comRegex = regexp.MustCompile(`^com_(.*)$`)
	var connectionErrorRegex = regexp.MustCompile(`^connection_errors_(.*)$`)
	var innodbRowRegex = regexp.MustCompile(`^innodb_rows_(.*)$`)
	var performanceSchemaRegex = regexp.MustCompile(`^performance_schema_(.*)$`)

	for row := range scrapes {
		name := strings.ToLower(row[0])
		value, err := strconv.ParseFloat(row[1], 64)
		if err != nil {
			// convert/serve text values here ?
			continue
		}

		command := comRegex.FindStringSubmatch(name)
		connectionError := connectionErrorRegex.FindStringSubmatch(name)
		innodbRow := innodbRowRegex.FindStringSubmatch(name)
		performanceSchema := performanceSchemaRegex.FindStringSubmatch(name)

		switch {
		case len(command) > 0:
			e.commands.With(prometheus.Labels{"command": command[1]}).Set(value)
		case len(connectionError) > 0:
			e.connectionErrors.With(prometheus.Labels{"error": connectionError[1]}).Set(value)
		case len(innodbRow) > 0:
			e.innodbRows.With(prometheus.Labels{"operation": innodbRow[1]}).Set(value)
		case len(performanceSchema) > 0:
			e.performanceSchema.With(prometheus.Labels{"instrumentation": performanceSchema[1]}).Set(value)
		default:
			e.setGenericMetric(name, value)
		}
	}
}

func (e *Exporter) collectMetrics(metrics chan<- prometheus.Metric) {
	for _, m := range e.metrics {
		m.Collect(metrics)
	}
}

func parseStatus(data []byte) string {
	logRexp := regexp.MustCompile(`\.([0-9]+$)`)
	logNum := logRexp.Find(data)

	switch {

	case string(data) == "Yes":
		return "1"

	case string(data) == "No":
		return "0"

	case len(logNum) > 1:
		return string(logNum[1:])

	default:
		return string(data)

	}
}

func main() {
	flag.Parse()

	dsn := os.Getenv("DATA_SOURCE_NAME")
	if len(dsn) == 0 {
		log.Fatal("couldn't find environment variable DATA_SOURCE_NAME")
	}

	exporter := NewMySQLExporter(dsn)
	prometheus.MustRegister(exporter)
	http.Handle(*metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
<head><title>MySQLd exporter</title></head>
<body>
<h1>MySQLd exporter</h1>
<p><a href='` + *metricPath + `'>Metrics</a></p>
</body>
</html>
`))
	})

	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
