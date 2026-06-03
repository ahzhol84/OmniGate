// pkg/command/router.go
// 全局命令路由器：管理所有支持下行指令的插件，并将指令分发到正确的插件。
package command

import (
	"context"
	"fmt"
	"iot-middleware/pkg/base"
	"log"
	"strings"
	"sync"
)

var (
	mu       sync.RWMutex
	handlers []registeredHandler
)

type registeredHandler struct {
	Name    string
	Handler base.ICommandable
}

// Register 注册一个支持下行指令的插件。
// 在 main.go 初始化阶段，对每个实现了 ICommandable 的 worker 调用本函数。
func Register(h base.ICommandable) {
	RegisterNamed("", h)
}

// RegisterNamed 注册一个支持下行指令的插件，并附带插件名用于定向路由。
func RegisterNamed(name string, h base.ICommandable) {
	mu.Lock()
	defer mu.Unlock()
	handlers = append(handlers, registeredHandler{Name: name, Handler: h})
	if name == "" {
		log.Printf("[COMMAND] 注册命令处理插件(未命名), 当前共 %d 个", len(handlers))
		return
	}
	log.Printf("[COMMAND] 注册命令处理插件 name=%s, 当前共 %d 个", name, len(handlers))
}

// Dispatch 将指令分发给第一个认领该设备的插件。
// 返回: 成功时返回 (*CommandReply, nil)；没有插件处理则返回 (nil, error)。
func Dispatch(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	return DispatchWithPlugin(ctx, deviceID, cmd, "")
}

// DispatchWithPlugin 将指令分发到指定插件（pluginID）或回退至自动认领。
// - pluginID 非空：先仅尝试该插件；若该插件不存在或返回 ErrCommandNotSupported，再回退自动认领。
// - pluginID 为空：直接按自动认领顺序分发。
func DispatchWithPlugin(ctx context.Context, deviceID string, cmd *base.DeviceCommand, pluginID string) (*base.CommandReply, error) {
	mu.RLock()
	hs := make([]registeredHandler, len(handlers))
	copy(hs, handlers)
	mu.RUnlock()

	pluginID = normalizePluginID(pluginID)

	if pluginID != "" {
		for _, h := range hs {
			if normalizePluginID(h.Name) != pluginID {
				continue
			}
			reply, err := h.Handler.SendCommand(ctx, deviceID, cmd)
			if err == base.ErrCommandNotSupported {
				break
			}
			return reply, err
		}
	}

	for _, h := range hs {
		reply, err := h.Handler.SendCommand(ctx, deviceID, cmd)
		if err == base.ErrCommandNotSupported {
			continue
		}
		// 已被处理（成功或插件级错误），直接返回
		return reply, err
	}

	return nil, fmt.Errorf("没有插件接受设备 %q 的指令", deviceID)
}

func normalizePluginID(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}
