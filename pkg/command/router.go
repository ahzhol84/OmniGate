// pkg/command/router.go
// 全局命令路由器：管理所有支持下行指令的插件，并将指令分发到正确的插件。
package command

import (
	"context"
	"fmt"
	"iot-middleware/pkg/base"
	"log"
	"sync"
)

var (
	mu       sync.RWMutex
	handlers []base.ICommandable
)

// Register 注册一个支持下行指令的插件。
// 在 main.go 初始化阶段，对每个实现了 ICommandable 的 worker 调用本函数。
func Register(h base.ICommandable) {
	mu.Lock()
	defer mu.Unlock()
	handlers = append(handlers, h)
	log.Printf("[COMMAND] 注册命令处理插件, 当前共 %d 个", len(handlers))
}

// Dispatch 将指令分发给第一个认领该设备的插件。
// 返回: 成功时返回 (*CommandReply, nil)；没有插件处理则返回 (nil, error)。
func Dispatch(ctx context.Context, deviceID string, cmd *base.DeviceCommand) (*base.CommandReply, error) {
	mu.RLock()
	hs := make([]base.ICommandable, len(handlers))
	copy(hs, handlers)
	mu.RUnlock()

	for _, h := range hs {
		reply, err := h.SendCommand(ctx, deviceID, cmd)
		if err == base.ErrCommandNotSupported {
			continue
		}
		// 已被处理（成功或插件级错误），直接返回
		return reply, err
	}

	return nil, fmt.Errorf("没有插件接受设备 %q 的指令", deviceID)
}
