// pkg/plugin/registry.go
package plugin

import "iot-middleware/pkg/base"

// Plugins 存储所有已注册的插件工厂函数
var Plugins = make(map[string]func() base.IWorker)

// Register 注册插件（通常在 init() 中调用）
func Register(name string, factory func() base.IWorker) {
	Plugins[name] = factory
}
