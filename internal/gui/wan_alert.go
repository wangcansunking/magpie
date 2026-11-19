package gui

import (
	"context"
	"time"

	"github.com/yetone/magpie/internal/wan"
)

type wanRecoveryAlert struct{ notified bool }

func (a *wanRecoveryAlert) observe(s wan.Status) bool {
	failed := s.State == "failed" && s.Error == wan.RelayRecoveryTimeoutMessage
	notify := failed && !a.notified
	a.notified = failed
	return notify
}

// Only cached status is read. One persistent failure produces one notice;
// leaving that failure arms a later independent recovery failure.
func watchWANRecovery(ctx context.Context, ticks <-chan time.Time, read func() wan.Status, send func()) {
	var alert wanRecoveryAlert
	for ctx.Err() == nil {
		if alert.observe(read()) && ctx.Err() == nil {
			send()
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
	}
}

func wanRecoveryAlertText(lang string) (string, string) {
	switch lang {
	case "zh":
		return "WAN 连接已断开", "已每 20 秒重试一次，连续 5 分钟仍未恢复。请检查网络后重新启动 WAN。"
	case "zh-TW":
		return "WAN 連線已中斷", "已每 20 秒重試一次，連續 5 分鐘仍未恢復。請檢查網路後重新啟動 WAN。"
	case "ja":
		return "WAN 接続が切断されました", "20 秒ごとに 5 分間再接続を試みましたが、復旧しませんでした。ネットワークを確認して WAN を再起動してください。"
	case "de":
		return "WAN-Verbindung unterbrochen", "Alle 20 Sekunden wurde ein neuer Versuch gestartet. Nach 5 Minuten ist WAN weiterhin offline. Prüfe das Netzwerk und starte WAN erneut."
	default:
		return "WAN disconnected", "Reconnection failed for 5 minutes with attempts every 20 seconds. Check the network and start WAN again."
	}
}
