package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync"
	"time"

	"github.com/928799934/googleAuthenticator"
	gxsync "github.com/dubbogo/gost/sync"
	gxtime "github.com/dubbogo/gost/time"
	"github.com/garyburd/redigo/redis"
	"github.com/hashicorp/consul/api"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

const (
	maxWheelTimeSpan    = 900e9  // 900s, 15 minute
	timeSpan            = 1000e6 // 100ms
	taskPoolQueueLength = 128
	taskPoolQueueNumber = 16
	taskPoolSize        = 2000
)

var (
	cfgPath    string
	version    bool
	versionStr = "unknow"
	addr       string
	consulAddr string
	wheel      *gxtime.Wheel
	taskPool   *gxsync.TaskPool
	err        error
	mu         sync.Mutex

	config     []configItem
	configYaml []byte
	taskMap    sync.Map
	taskList   = make(map[string]func(), 0)

	secret string

	consulConfigKey = "redis-exporter/config"
)

type configItem struct {
	Connstr string
	Monitor map[string]redisMetrics
}

type redisMetrics struct {
	Command []string
	Desc    string
}

func init() {
	//读取命令行参数
	flag.StringVar(&cfgPath, "config", "./config/config.yaml", "config file path")
	flag.StringVar(&consulAddr, "consul", "", "consule addr")
	flag.BoolVar(&version, "v", false, "version")
	flag.StringVar(&addr, "addr", ":8081", "The address to listen on for HTTP requests.")
	flag.StringVar(&secret, "secret", "", "api requests auth secret.")

	buckets := maxWheelTimeSpan / timeSpan
	wheel = gxtime.NewWheel(time.Duration(timeSpan), int(buckets)) //wheel longest span is 15 minute
	taskPool = gxsync.NewTaskPool(
		gxsync.WithTaskPoolTaskQueueNumber(taskPoolQueueNumber), //tQNumber 切片长度16，类型是chan类型
		gxsync.WithTaskPoolTaskQueueLength(taskPoolQueueLength), //tQLen chan缓冲128
		gxsync.WithTaskPoolTaskPoolSize(taskPoolSize),           //tQPoolSize 起2000个worker消费，通过taskPoolSize%taskPoolQueueNumber消费对应的channel
	)
}

func main() {
	//解析命令行参数
	flag.Parse()
	if version == true {
		fmt.Println(versionStr)
		os.Exit(0)
	}
	//解析配置文件
	if err := initConfig(); err != nil {
		return
	}

	//创建任务
	dispatchTask()

	//metrics server && pprof
	go func() {
		fmt.Println("server run...")
		http.Handle("/metrics", promhttp.Handler())
		http.Handle("/reload", http.HandlerFunc(reloadConfig))
		http.Handle("/exit", http.HandlerFunc(exit))
		logrus.Fatal(http.ListenAndServe(addr, nil))
	}()

	//执行任务
	for {
		select {
		case <-wheel.After(5 * time.Second):
			mu.Lock()
			taskMap.Range(func(k, v interface{}) bool {
				taskPool.AddTask(taskList[k.(string)])
				return true
			})
			mu.Unlock()
		}
	}
}

func initConfig() error {
	var err error
	if consulAddr == "" {
		configYaml, err = ioutil.ReadFile(cfgPath)
	} else {
		configYaml, err = getConfigByConsul(consulAddr, consulConfigKey)
	}
	if err != nil {
		logrus.Warningf("Read config fail:%s", err)
		return err
	}
	err = yaml.Unmarshal(configYaml, &config)
	if err != nil {
		logrus.Warningf("Unmarshal config file fail:%s", err)
		return err
	}
	return nil
}

func dispatchTask() {
	mu.Lock()
	defer mu.Unlock()
	logrus.Info("dispatch task")
	taskMap = sync.Map{}
	//prometheus rgistry
	registry := prometheus.DefaultRegisterer
	for i := 0; i < len(config); i++ {
		item := config[i]
		//初始化连接池
		connstr := item.Connstr
		redisPools := getReisPool(connstr)
		//
		for k, v := range item.Monitor {
			desc := v.Desc
			//记录指标监控运行状态，0运行，1,暂停，2停止(后续实现此功能)
			taskMap.Store(k, 0)
			//获取要执行的redis命令及参数
			command := v.Command[0]
			args := make([]interface{}, 0)
			for j := 1; j < len(v.Command); j++ {
				args = append(args, v.Command[j])
			}
			//创建metrics
			gaugeMetrics := prometheus.NewGaugeVec(
				prometheus.GaugeOpts{
					Name: k,
					Help: desc,
				},
				[]string{"desc"},
			)

			registry.Unregister(gaugeMetrics)
			registry.Register(gaugeMetrics)
			//redisPools必包形式引用了父级变量
			taskList[k] = func() {
				pool := redisPools.Get()
				defer pool.Close()
				ret, err := pool.Do(command, args...)
				if err != nil {
					logrus.Errorf("%v %v", err, args)
					return
				}
				switch v := ret.(type) {
				case []byte:
					logrus.Warnf("%s %v %v", command, args, string(v))
				case int64:
					gaugeMetrics.WithLabelValues(desc).Set(float64(v))
					//logrus.Info(v)
				default:
					logrus.Warnf("%s %v %v", command, args, v)
				}
			}
		}
	}
}

// 重新加载配置文件
func reloadConfig(w http.ResponseWriter, r *http.Request) {
	err := authCode(r)
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}
	logrus.Info("reload config")
	if err := initConfig(); err != nil {
		fmt.Fprintln(w, err)
		return
	}
	dispatchTask()
	fmt.Fprintln(w, "reload config success")
}

// 退出程序
func exit(w http.ResponseWriter, r *http.Request) {
	err := authCode(r)
	if err != nil {
		fmt.Fprintln(w, err)
		return
	}

	fmt.Fprintln(w, "bye bye!")
	logrus.Fatal("exit!")
}

func authCode(r *http.Request) (err error) {
	if secret != "" {
		code := r.URL.Query().Get("code")
		if code == "" {
			return fmt.Errorf("%s", "code empty!")
		}
		ga := googleAuthenticator.NewGAuth()
		ret, err := ga.VerifyCode(secret, code, 1)
		if err != nil {
			return err
		}
		if !ret {
			return fmt.Errorf("%s", "code auth fail!")
		}
	}
	return nil
}

// 从consule对应的key配置
func getConfigByConsul(addr string, key string) ([]byte, error) {
	var ret []byte
	client, err := api.NewClient(&api.Config{Address: addr})
	if err != nil {
		logrus.Error(err)
		return nil, err
	}

	// Get a handle to the KV API
	kv := client.KV()

	// Lookup the pair
	pair, _, err := kv.List(consulConfigKey, nil)
	if err != nil {
		logrus.Error(err)
		return nil, err
	}
	if len(pair) == 0 {
		return nil, fmt.Errorf("从%s获取%s值为nil", addr, key)
	}

	for i := 0; i < len(pair); i++ {
		ret = append(ret, pair[i].Value...)
		ret = append(ret, 13)
		//fmt.Println(string(pair[i].Value))
	}
	return ret, nil
}

// 根据连接串获取RedisPool
func getReisPool(connstr string) redis.Pool {
	dialOption := []redis.DialOption{
		redis.DialConnectTimeout(time.Duration(3) * time.Second),
		redis.DialReadTimeout(time.Duration(5) * time.Second),
		redis.DialWriteTimeout(time.Duration(5) * time.Second),
	}
	pool := redis.Pool{
		MaxIdle:     100,
		IdleTimeout: 250 * time.Second,
		Dial: func() (redis.Conn, error) {
			c, err := redis.DialURL(connstr, dialOption...)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("ping")
			return err
		},
	}
	return pool
}
