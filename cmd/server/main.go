// cmd/server/main.go
package main

import (
	"context"
	"encoding/json"
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/common"
	"iot-middleware/pkg/plugin"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"gopkg.in/yaml.v3"

	_ "iot-middleware/plugins/aiqiangua_x8"
	_ "iot-middleware/plugins/dyf20a_poller"
	_ "iot-middleware/plugins/generic_http_listener"
	_ "iot-middleware/plugins/likaan_push_listener"
	_ "iot-middleware/plugins/simple_http_responder"
	_ "iot-middleware/plugins/sxb_poller"
)

func main() {
	cfg := loadConfig()

	// 初始化全局资源
	common.InitRedis(cfg.Global.RedisAddr)
	common.InitDB(cfg.Global.DBDsn)

	// 数据通道(传送带,暂存100条数据)
	dataChan := make(chan *base.DeviceData, 100)
	writerDone := make(chan struct{})
	go func() {
		common.StartDataWriter(dataChan)
		close(writerDone)
	}()

	// 按插件名称分组配置
	pluginConfigs := make(map[string][]json.RawMessage)

	log.Println("🔍 开始解析插件配置...")
	for _, pCfg := range cfg.Plugins {
		if !pCfg.Enabled {
			log.Printf("⚠️ 插件未启用: %s", pCfg.Name)
			continue
		}

		// 处理多个配置项
		var configsToProcess []yaml.Node
		if len(pCfg.Configs) > 0 {
			// 新格式：使用configs字段
			configsToProcess = pCfg.Configs
		} else if !pCfg.Config.IsZero() {
			// 兼容旧格式：使用config字段
			configsToProcess = []yaml.Node{pCfg.Config}
		} else {
			log.Printf("❌ 插件 %s 没有配置项", pCfg.Name)
			continue
		}

		// 解析每个配置项
		for i, yamlConfig := range configsToProcess {
			var configMap map[string]interface{}
			if err := yamlConfig.Decode(&configMap); err != nil {
				log.Printf("❌ 插件 %s 第%d个配置解码失败: %v", pCfg.Name, i+1, err)
				continue
			}
			rawConfig, err := json.Marshal(configMap)
			if err != nil {
				log.Printf("❌ 插件 %s 第%d个配置转 JSON 失败: %v", pCfg.Name, i+1, err)
				continue
			}

			pluginConfigs[pCfg.Name] = append(pluginConfigs[pCfg.Name], rawConfig)
			log.Printf("✅ 插件 %s 第%d个配置解析成功", pCfg.Name, i+1)
		}
	}

	// 启动插件（每个插件一个 goroutine）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workerWG sync.WaitGroup

	log.Printf("🚀 开始启动插件，共 %d 个插件类型...", len(pluginConfigs))

	for name, configs := range pluginConfigs {
		factory, exists := plugin.Plugins[name]
		if !exists {
			log.Fatalf("❌ 插件未注册: %s", name)
		}

		worker := factory()
		if err := worker.Init(configs); err != nil {
			log.Printf("⚠️ 插件 %s 初始化失败: %v", name, err)
			continue
		}

		workerWG.Add(1)
		go func(pluginName string, w base.IWorker) {
			defer workerWG.Done()
			w.Start(ctx, dataChan)
			log.Printf("🧹 插件 %s 已退出", pluginName)
		}(name, worker)
		log.Printf("✅ 插件 %s 已启动（共 %d 个配置项）", name, len(configs))
	}

	// 等待中断信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("🛑 收到中断信号，正在关闭...")
	cancel()
	workerWG.Wait()
	close(dataChan)
	<-writerDone
	log.Println("✅ 数据已刷盘，服务已完成优雅退出")
}
