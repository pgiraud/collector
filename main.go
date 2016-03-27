package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	flag "github.com/ogier/pflag"

	"database/sql"

	_ "github.com/lib/pq" // Enable database package to use Postgres

	"github.com/pganalyze/collector/config"
	"github.com/pganalyze/collector/dbstats"
	"github.com/pganalyze/collector/explain"
	"github.com/pganalyze/collector/logs"
	scheduler "github.com/pganalyze/collector/scheduler"
	systemstats "github.com/pganalyze/collector/systemstats"
	"github.com/pganalyze/collector/util"
)

type snapshot struct {
	ActiveQueries []dbstats.Activity          `json:"backends"`
	Statements    []dbstats.Statement         `json:"queries"`
	Postgres      snapshotPostgres            `json:"postgres"`
	System        *systemstats.SystemSnapshot `json:"system"`
	Logs          []logs.Line                 `json:"logs"`
	Explains      []explain.Explain           `json:"explains"`
}

type snapshotPostgres struct {
	Relations []dbstats.Relation `json:"schema"`
}

func collectStatistics(config config.DatabaseConfig, db *sql.DB, submitCollectedData bool, logger *util.Logger) (err error) {
	var stats snapshot
	var explainInputs []explain.ExplainInput

	stats.ActiveQueries, err = dbstats.GetActivity(db)
	if err != nil {
		return err
	}

	stats.Statements, err = dbstats.GetStatements(db)
	if err != nil {
		return err
	}

	stats.Postgres.Relations, err = dbstats.GetRelations(db)
	if err != nil {
		return err
	}

	stats.System = systemstats.GetSystemSnapshot(config)
	stats.Logs, explainInputs = logs.GetLogLines(config)

	stats.Explains = explain.RunExplain(db, explainInputs)

	statsJSON, _ := json.Marshal(stats)

	if !submitCollectedData {
		var out bytes.Buffer
		json.Indent(&out, statsJSON, "", "\t")
		logger.PrintInfo("Dry run - JSON data that would have been sent:\n%s", out.String())
		return
	}

	var compressedJSON bytes.Buffer
	w := zlib.NewWriter(&compressedJSON)
	w.Write(statsJSON)
	w.Close()

	resp, err := http.PostForm(config.APIURL, url.Values{
		"data":               {compressedJSON.String()},
		"data_compressor":    {"zlib"},
		"api_key":            {config.APIKey},
		"submitter":          {"pganalyze-collector 0.9.0rc1"},
		"system_information": {"false"},
		"no_reset":           {"true"},
		"query_source":       {"pg_stat_statements"},
		"collected_at":       {fmt.Sprintf("%d", time.Now().Unix())},
	})
	// TODO: We could consider re-running on error (e.g. if it was a temporary server issue)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Error when submitting: %s\n", body)
		return
	}

	logger.PrintInfo("Submitted snapshot successfully")
	return
}

func collectAllDatabases(databases []configAndConnection, submitCollectedData bool, logger *util.Logger) {
	for _, database := range databases {
		prefixedLogger := logger.WithPrefix(database.config.SectionName)
		err := collectStatistics(database.config, database.connection, submitCollectedData, prefixedLogger)
		if err != nil {
			prefixedLogger.PrintError("%s", err)
		}
	}
}

func connectToDb(config config.DatabaseConfig, logger *util.Logger) (*sql.DB, error) {
	connectString := config.GetPqOpenString()
	logger.PrintVerbose("sql.Open(\"postgres\", \"%s\")", connectString)

	db, err := sql.Open("postgres", connectString)
	if err != nil {
		return nil, err
	}

	err = db.Ping()
	if err != nil {
		return nil, err
	}

	return db, nil
}

type configAndConnection struct {
	config     config.DatabaseConfig
	connection *sql.DB
}

func establishConnection(config config.DatabaseConfig, logger *util.Logger) (database configAndConnection, err error) {
	database = configAndConnection{config: config}
	requestedSslMode := config.DbSslMode

	// Go's lib/pq does not support sslmode properly, so we have to implement the "prefer" mode ourselves
	if requestedSslMode == "prefer" {
		config.DbSslMode = "require"
	}

	database.connection, err = connectToDb(config, logger)
	if err != nil {
		if err.Error() == "pq: SSL is not enabled on the server" && requestedSslMode == "prefer" {
			config.DbSslMode = "disable"
			database.connection, err = connectToDb(config, logger)
		}
	}

	return
}

func run(wg sync.WaitGroup, testRun bool, submitCollectedData bool, logger *util.Logger, configFilename string) chan<- bool {
	var databases []configAndConnection

	schedulerGroups, err := scheduler.ReadSchedulerGroups(scheduler.DefaultConfig)
	if err != nil {
		logger.PrintError("Error: Could not read scheduler groups, awaiting SIGHUP or process kill")
		return nil
	}

	databaseConfigs, err := config.Read(configFilename)
	if err != nil {
		logger.PrintError("Error: Could not read configuration, awaiting SIGHUP or process kill")
		return nil
	}

	for _, config := range databaseConfigs {
		prefixedLogger := logger.WithPrefix(config.SectionName)
		database, err := establishConnection(config, prefixedLogger)
		if err != nil {
			prefixedLogger.PrintError("Error: Failed to connect to database: %s", err)
		} else {
			databases = append(databases, database)
		}
	}

	// We intentionally don't do a test-run in the normal mode, since we're fine with
	// a later SIGHUP that fixes the config (or a temporarily unreachable server at start)
	if testRun {
		collectAllDatabases(databases, submitCollectedData, logger)
		return nil
	}

	stop := schedulerGroups["stats"].Schedule(func() {
		wg.Add(1)
		collectAllDatabases(databases, submitCollectedData, logger)
		wg.Done()
	}, logger, "collection of all databases")

	return stop
}

func main() {
	var testRun bool
	var dryRun bool
	var submitCollectedData bool
	var configFilename string
	var pidFilename string

	logger := &util.Logger{Destination: log.New(os.Stderr, "", log.LstdFlags)}

	usr, err := user.Current()
	if err != nil {
		logger.PrintError("Could not get user context from operating system - can't initialize, exiting.")
		return
	}

	flag.BoolVarP(&testRun, "test", "t", false, "Tests whether we can successfully collect data, submits it to the server, and exits afterwards.")
	flag.BoolVarP(&logger.Verbose, "verbose", "v", false, "Outputs additional debugging information, use this if you're encoutering errors or other problems.")
	flag.BoolVar(&dryRun, "dry-run", false, "Print JSON data that would get sent to web service (without actually sending) and exit afterwards.")
	flag.StringVar(&configFilename, "config", usr.HomeDir+"/.pganalyze_collector.conf", "Specify alternative path for config file.")
	flag.StringVar(&pidFilename, "pidfile", "", "Specifies a path that a pidfile should be written to. (default is no pidfile being written)")
	flag.Parse()

	if dryRun {
		submitCollectedData = false
		testRun = true
	} else if testRun {
		submitCollectedData = true
	} else {
		submitCollectedData = true
	}

	if pidFilename != "" {
		pid := os.Getpid()
		err := ioutil.WriteFile(pidFilename, []byte(strconv.Itoa(pid)), 0644)
		if err != nil {
			logger.PrintError("Could not write pidfile to \"%s\" as requested, exiting.", pidFilename)
			return
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	wg := sync.WaitGroup{}

ReadConfigAndRun:
	stop := run(wg, testRun, submitCollectedData, logger, configFilename)
	if stop == nil {
		return
	}

	// Block here until we get any of the registered signals
	s := <-sigs

	// Stop the scheduled runs
	stop <- true

	if s == syscall.SIGHUP {
		logger.PrintInfo("Reloading configuration...")
		goto ReadConfigAndRun
	}

	signal.Stop(sigs)

	logger.PrintInfo("Exiting...")
	wg.Wait()
}
