package logmetrics

import (
	"io/ioutil"
	"launchpad.net/~niemeyer/goyaml/beta"
	"log"
	"log/syslog"
	"os"
	"strings"
	//"syseng/sre2"
	"regexp"
	"time"
)

type Config struct {
	pollInterval int
	pushPort     int
	pushWait     int
	pushHost     string
	pushProto    string
	pushType     string
	pushNumber   int
	stats_wait   int
	logGroups    map[string]*LogGroup
	logFacility  syslog.Priority
}

type KeyExtract struct {
	tag         string
	metric_type string
	key_suffix  string
	format      string
	multiply    int

	operations map[string][]int
}

type LogGroup struct {
	name             string
	globFiles        []string
	re               *regexp.Regexp
	expected_matches int

	date_position int
	date_format   string

	key_prefix string
	tags       map[string]int
	metrics    map[int]KeyExtract

	histogram_size                  int
	histogram_alpha_decay           float64
	histogram_rescale_threshold_min int
	ewmaInterval                    int

	goroutines        int
	workload_split_on int
	interval          int

	fail_operation_warn bool
	parse_from_start    bool
	fail_regex_warn     bool

	//Channels
	tail_data []chan []string

	//Workvars
	last_date_str string
	last_date     time.Time
}

func (lg *LogGroup) getNbTags() int {
	return len(lg.tags)
}

func (conf *Config) GetPusherNumber() int {
	return conf.pushNumber
}

func cleanSre2(log_group_name string, re string) (*regexp.Regexp, error) {
	//Little hack to support extended style regex. Removes comments, spaces en endline
	noSpacesRe := strings.Replace(re, " ", "", -1)
	splitRe := strings.Split(noSpacesRe, "\\n")

	var rebuiltRe []string
	for _, l := range splitRe {
		noComments := strings.Split(l, "#")
		rebuiltRe = append(rebuiltRe, string(noComments[0]))
	}
	cleanRe := strings.Join(rebuiltRe, "")

	log.Printf("Cleaned regex used for %s: %s", log_group_name, cleanRe)

	//Try to compile the regex
	if compiledRe, err := regexp.Compile(cleanRe); err == nil {
		return compiledRe, nil
	} else {
		return nil, err
	}
}

func parseMetrics(conf map[interface{}]interface{}) map[int]KeyExtract {
	keyExtracts := make(map[int]KeyExtract)

	for metric_type, metrics := range conf {
		switch m := metrics.(type) {
		case map[interface{}]interface{}:
			key_suffix := m["key_suffix"].(string)

			var format string
			var multiply int
			if format_key, ok := m["format"]; ok == true {
				format = format_key.(string)
			} else {
				format = "int"
			}
			if multiply_key, ok := m["multiply"]; ok == true {
				multiply = multiply_key.(int)
			} else {
				multiply = 1
			}

			for _, val := range m["data"].([]interface{}) {
				position := val.([]interface{})[0].(int)
				tag := val.([]interface{})[1].(string)

				operations := make(map[string][]int)
				if len(val.([]interface{})) > 2 {
					operations_struct := val.([]interface{})[2].(map[interface{}]interface{})

					for op, opvals := range operations_struct {
						//Make sure we only accept operation we can perform
						if op != "add" && op != "sub" {
							log.Fatalf("Operation %s no supported", op)
						}

						for _, opval := range opvals.([]interface{}) {
							operations[op.(string)] = append(operations[op.(string)], opval.(int))
						}
					}
				}

				keyExtracts[position] = KeyExtract{tag: tag, metric_type: metric_type.(string),
					key_suffix: key_suffix, format: format, multiply: multiply, operations: operations}
			}
		}

	}

	return keyExtracts
}

func LoadConfig(configFile string) Config {
	byteConfig, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}

	var rawCfg interface{}
	err = goyaml.Unmarshal(byteConfig, &rawCfg)
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}

	settings := rawCfg.(map[interface{}]interface{})["settings"]

	var cfg Config
	cfg.logGroups = make(map[string]*LogGroup)

	//Settings
	for key, val := range settings.(map[interface{}]interface{}) {
		switch v := val.(type) {
		case int:
			switch key {
			case "poll_interval":
				cfg.pollInterval = v
			case "push_port":
				cfg.pushPort = v
			case "push_wait":
				cfg.pushWait = v
			case "push_number":
				cfg.pushNumber = v
			case "stats_wait":
				cfg.stats_wait = v

			default:
				log.Fatalf("Unknown key settings.%s", key)
			}

		case string:
			switch key {
			case "log_facility":
				//Lookup
				if facility, found := facilityStrings[v]; found == true {
					cfg.logFacility = facility
				} else {
					log.Fatalf("Unable to map log_facility: %s", v)
				}
			case "push_host":
				cfg.pushHost = v
			case "push_proto":
				cfg.pushProto = v
			case "push_type":
				cfg.pushType = v

			default:
				log.Fatalf("Unknown key settings.%s", key)
			}

		default:
			log.Fatalf("Unknown key settings.%s", key)
		}
	}

	//Some default vals
	if cfg.pollInterval == 0 {
		cfg.pollInterval = 15
	}
	if cfg.logFacility == 0 {
		cfg.logFacility = syslog.LOG_LOCAL0
	}
	if cfg.pushHost == "" {
		cfg.pushHost = "localhost"
	}
	if cfg.pushProto == "" {
		cfg.pushProto = "udp"
	}
	if cfg.pushType == "" {
		cfg.pushType = "tcollector"
	}
	if cfg.pushNumber == 0 {
		cfg.pushNumber = 1
	}
	if cfg.stats_wait == 0 {
		cfg.stats_wait = 60
	}

	//Log_groups configs
	for name, group_content := range rawCfg.(map[interface{}]interface{}) {
		//Skip settings, already parsed
		if name == "settings" {
			continue
		}

		var lg LogGroup

		lg.name = name.(string)
		lg.tags = make(map[string]int)

		//Process content
		for key, val := range group_content.(map[interface{}]interface{}) {
			switch v := val.(type) {
			case string:
				switch key {
				case "re":
					var err error
					if lg.re, err = cleanSre2(lg.name, v); err != nil {
						log.Fatal(err)
					}
				case "key_prefix":
					lg.key_prefix = v

				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			case int:
				switch key {
				case "interval":
					lg.interval = v
				case "ewma_interval":
					lg.ewmaInterval = v
				case "expected_matches":
					lg.expected_matches = v
				case "histogram_size":
					lg.histogram_size = v
				case "goroutines":
					lg.goroutines = v
				case "histogram_rescale_threshold_min":
					lg.histogram_rescale_threshold_min = v
				case "workload_split_on":
					lg.workload_split_on = v

				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			case float64:
				switch key {
				case "histogram_alpha_decay":
					lg.histogram_alpha_decay = v

				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			case bool:
				switch key {
				case "warn_on_regex_fail":
					lg.fail_regex_warn = v
				case "parse_from_start":
					lg.parse_from_start = v
				case "warn_on_operation_fail":
					lg.fail_operation_warn = v
				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			case []interface{}:
				switch key {
				case "files":
					for _, file := range v {
						lg.globFiles = append(lg.globFiles, file.(string))
					}

				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			case map[interface{}]interface{}:
				switch key {
				case "tags":
					for tag, pos := range v {
						lg.tags[tag.(string)] = pos.(int)
					}

				case "metrics":
					lg.metrics = parseMetrics(v)

				case "date":
					for date_name, date_val := range v {
						if date_name.(string) == "position" {
							lg.date_position = date_val.(int)
						} else if date_name.(string) == "format" {
							lg.date_format = date_val.(string)
						} else {
							log.Fatalf("Unknown key %s.date.%s", name, date_name)
						}
					}

				default:
					log.Fatalf("Unknown key %s.%s", name, key)
				}

			default:
				log.Fatalf("Unknown key %s.%s", name, key)
			}
		}

		//Defaults
		if lg.goroutines == 0 {
			lg.goroutines = 1
		}
		if lg.histogram_alpha_decay == 0 {
			lg.histogram_alpha_decay = 0.15
		}
		if lg.histogram_size == 0 {
			lg.histogram_size = 512
		}
		if lg.histogram_rescale_threshold_min == 0 {
			lg.histogram_rescale_threshold_min = 60
		}

		//Init channels
		lg.tail_data = make([]chan []string, lg.goroutines)
		for i := 0; i < lg.goroutines; i++ {
			lg.tail_data[i] = make(chan []string, 1000)
		}

		cfg.logGroups[name.(string)] = &lg
	}

	return cfg
}