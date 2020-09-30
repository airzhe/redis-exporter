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

	gxsync "github.com/dubbogo/gost/sync"
	gxtime "github.com/dubbogo/gost/time"
	"github.com/garyburd/redigo/redis"
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
	wheel      *gxtime.Wheel
	taskPool   *gxsync.TaskPool

	config   []configItem
	taskMap  sync.Map
	taskList = make(map[string]func(), 0)
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
	flag.BoolVar(&version, "v", false, "version")
	flag.StringVar(&addr, "addr", ":8080", "The address to listen on for HTTP requests.")

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
	file, err := ioutil.ReadFile(cfgPath)
	if err != nil {
		logrus.Fatalf("Read config fail:%s", err)
	}
	err = yaml.Unmarshal(file, &config)
	if err != nil {
		logrus.Fatalf("Fatal error config file read fail:%s", err)
	}
	//prometheus rgistry
	registry := prometheus.DefaultRegisterer
	for i := 0; i < len(config); i++ {
		item := config[i]
		//初始化连接池
		connstr := item.Connstr
		redisPools := getReisPool(connstr)
		//
		for k, v := range item.Monitor {
			//记录指标监控运行状态，0运行，1,暂停，2停止(后续实现此功能)
			taskMap.Store(k, 0)
			//获取要执行的redis命令及参数
			command := v.Command[0]
			args := make([]interface{}, 0)
			for j := 1; j < len(v.Command); j++ {
				args = append(args, v.Command[j])
			}
			//创建metrics
			gaugeMetrics := prometheus.NewGauge(
				prometheus.GaugeOpts{
					Name: k,
					Help: v.Desc,
				},
			)
			registry.MustRegister(gaugeMetrics)
			//redisPools必包形式引用了父级变量
			taskList[k] = func() {
				pool := redisPools.Get()
				ret, err := pool.Do(command, args...)
				if err != nil {
					logrus.Errorf("%v %v", err, args)
					return
				}
				switch v := ret.(type) {
				case []byte:
					logrus.Warnf("%s %v %v", command, args, string(v))
				case int64:
					gaugeMetrics.Set(float64(v))
					//logrus.Info(v)
				default:
					logrus.Warnf("%s %v %v", command, args, v)
				}
			}
		}
	}

	//metrics server && pprof
	go func() {
		fmt.Println("server run...")
		http.Handle("/metrics", promhttp.Handler())
		logrus.Fatal(http.ListenAndServe(addr, nil))
	}()

	//执行任务
	for {
		select {
		case <-wheel.After(5 * time.Second):
			taskMap.Range(func(k, v interface{}) bool {
				taskPool.AddTask(taskList[k.(string)])
				return true
			})
		}
	}
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
