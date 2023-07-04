package main

import (
	"fmt"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/api"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/channel"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/config"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/filter"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/input"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/metrics"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/output"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/position"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/rule"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/schema"
	"github.com/liuxinwang/go-mysql-starrocks/pkg/utils"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sevlyar/go-daemon"
	"github.com/siddontang/go-log/log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 输入参数处理
	help := utils.HelpInit()
	// 日志初始化
	// _ = utils.LogInit(help)
	// 目前使用daemon的 log
	log.SetLevelByName(*help.LogLevel)
	// daemon模式启动
	if *help.Daemon {
		cntxt := &daemon.Context{
			PidFileName: utils.GetExecPath() + "/go_mysql_sr.pid",
			PidFilePerm: 0644,
			LogFileName: *help.LogFile,
			LogFilePerm: 0640,
			WorkDir:     "./",
			Umask:       027,
		}
		d, err := cntxt.Reborn()
		if err != nil {
			log.Fatal("Unable to run: ", err)
		}

		if d != nil {
			return
		}
		defer func(cntxt *daemon.Context) {
			err := cntxt.Release()
			if err != nil {
				log.Fatal("daemon release error: ", err)
			}
		}(cntxt)
	}

	// 进程信号处理
	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		os.Kill,
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	// Start prometheus http monitor
	go func() {
		metrics.OpsStartTime.Set(float64(time.Now().Unix()))
		log.Infof("starting http on port %d.", *help.HttpPort)
		http.Handle("/metrics", promhttp.Handler())
		httpPortAddr := fmt.Sprintf(":%d", *help.HttpPort)
		err := http.ListenAndServe(httpPortAddr, nil)
		if err != nil {
			log.Fatalf("starting http monitor error: %v", err)
		}
	}()

	// 初始化配置
	baseConfig := config.NewBaseConfig(help.ConfigFile)

	var isc config.InputSourceConfig
	var otc config.OutputTargetConfig
	var ip input.Plugin
	var rr rule.Rule
	var pos position.Position
	var syncChan *channel.SyncChannel
	var outputChan *channel.OutputChannel
	var matcherFilter filter.MatcherFilter
	var oo output.Plugin
	var inSchema schema.Schema
	var outSchema schema.Schema

	// 初始化channel
	syncChan = &channel.SyncChannel{}
	syncChan.NewChannel(baseConfig.SyncParamConfig)
	outputChan = &channel.OutputChannel{}
	outputChan.NewChannel(baseConfig.SyncParamConfig)

	// 初始化output插件配置
	switch baseConfig.OutputConfig.Type {
	case "doris":
		otc = &config.DorisConfig{}
		// 初始化rule配置
		rr = &rule.DorisRules{}
		// 初始化output插件实例
		oo = &output.Doris{}
		// init output schema
		outSchema = &schema.DorisTables{}
	case "starrocks":
		otc = &config.StarrocksConfig{}
		// 初始化rule配置
		rr = &rule.StarrocksRules{}
		// 初始化output插件实例
		oo = &output.Starrocks{}
		// init output schema
		outSchema = &schema.StarrocksTables{}
	}
	otc.NewOutputTargetConfig(baseConfig.OutputConfig.Config)
	outSchema.NewSchemaTables(baseConfig.OutputConfig.Config["target"])
	rr.NewRule(baseConfig.OutputConfig.Config)

	// 初始化input插件配置
	switch baseConfig.InputConfig.Type {
	case "mysql":
		isc = &config.MysqlConfig{}
		// 初始化input插件实例
		ip = &input.MysqlInputPlugin{}
		pos = &position.MysqlPosition{}
		// init input schema
		inSchema = &schema.MysqlTables{}
	case "mongo":
		isc = &config.MongoConfig{}
		// 初始化input插件实例
		ip = &input.MongoInputPlugin{}
		pos = &position.MongoPosition{}
		// init input schema
		inSchema = &schema.MongoTables{}
	}
	isc.NewInputSourceConfig(baseConfig.InputConfig.Config)
	inSchema.NewSchemaTables(baseConfig.InputConfig.Config["source"])

	oo.NewOutput(otc, rr.GetRuleToMap(), inSchema, outSchema)
	ip.NewInput(isc, rr.GetRuleToRegex())
	pos.LoadPosition(baseConfig)

	// 初始化filter配置
	matcherFilter = filter.NewMatcherFilter(baseConfig.FilterConfig)

	// 启动input插件
	pos = ip.StartInput(pos, syncChan)
	// 启动position
	pos.StartPosition()
	// 启动filter
	matcherFilter.StartFilter(syncChan, outputChan, inSchema)
	// 启动output插件
	go oo.StartOutput(outputChan)

	// api handle
	http.HandleFunc("/api/addRule", api.AddRuleHandle(ip, oo))
	http.HandleFunc("/api/delRule", api.DelRuleHandle(ip, oo))
	http.HandleFunc("/api/getRule", api.GetRuleHandle(oo))

	select {
	case n := <-sc:
		log.Infof("receive signal %v, closing", n)
		// 关闭input插件
		ip.Close()
		// 关闭filter
		syncChan.Close()
		// 关闭output插件
		oo.Close()
		// flush last position
		pos.Close()
		// close schema conn
		inSchema.Close()
		outSchema.Close()
		log.Infof("[Main] is stopped.")
	}
}
