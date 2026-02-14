// plugins/dyf20a_poller/plugin.go
package dyf20a_poller

import (
	"iot-middleware/pkg/base"
	"iot-middleware/pkg/plugin"
)

func init() {
	plugin.Register("dyf20a_poller", func() base.IWorker {
		return &DYF20AWorker{}
	})
}
