package rainbow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/caoyingjunz/rainbow/pkg/util"
	"io"
	"k8s.io/klog/v2"
	"net/http"
	"time"

	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/db/model/rainbow"
	"github.com/caoyingjunz/rainbow/pkg/types"
)

type PushMessage struct {
	Text    map[string]string `json:"text"`
	Msgtype string            `json:"msgtype"`
}

func (s *ServerController) preCreateNotify(ctx context.Context, req *types.CreateNotificationRequest) error {
	return nil
}

func (s *ServerController) CreateNotify(ctx context.Context, req *types.CreateNotificationRequest) error {
	if err := s.preCreateNotify(ctx, req); err != nil {
		return err
	}

	pushCfg, err := req.PushCfg.Marshal()
	if err != nil {
		return err
	}
	_, err = s.factory.Notify().Create(ctx, &model.Notification{
		Name:   req.Name,
		Role:   req.Role,
		Enable: req.Enable,
		Type:   req.Type,
		UserModel: rainbow.UserModel{
			UserId:   req.UserId,
			UserName: req.UserName,
		},
		PushCfg:   pushCfg,
		ShortDesc: req.ShortDesc,
	})
	if err != nil {
		klog.Error("创建推送(%s)记录失败: %v", req.Name, err)
	}

	return err
}

func (s *ServerController) SendNotify(ctx context.Context, req *types.SendNotificationRequest) error {
	list, err := s.factory.Notify().List(ctx, db.WithUser(req.UserId), db.WithEnable(1))
	if err != nil {
		return fmt.Errorf("failed to query notification configs: %w", err)
	}
	if len(list) == 0 {
		klog.Warningf("no enabled notification config found for user %s", req.UserId)
		return nil
	}

	switch req.Role {
	case 1:
		return s.SendRegisterNotify(ctx, req)
	case 0:
		//for _, n := range list {
		//	if err := s.sendByType(ctx, &n, req.Content); err != nil {
		//		klog.Errorf("failed to send notification via %s (ID: %d): %v", n.Type, n.Id, err)
		//	}
		//}
	}

	return nil
}

func (s *ServerController) sendByType(ctx context.Context, n *model.Notification, msg string) error {
	switch n.Type {
	case "wecom", "dingding", "feishu": // 支持多个类型
		return s.sendWebhook(ctx, n, msg)
	case "email":
		// TODO: 实现邮件推送
	default:
		klog.Warningf("unknown notification type: %s (ID: %d)", n.Type, n.Id)
	}
	return nil
}

// 通用 webhook 推送方法
func (s *ServerController) sendWebhook(ctx context.Context, n *model.Notification, msg string) error {
	var cfg struct {
		URL string `json:"url"`
	}

	if err := json.Unmarshal([]byte(n.PushCfg), &cfg); err != nil {
		return fmt.Errorf("invalid %s config JSON (ID: %d): %w", n.Type, n.Id, err)
	}
	if cfg.URL == "" {
		return fmt.Errorf("empty %s URL (ID: %d)", n.Type, n.Id)
	}

	payload := map[string]interface{}{
		"msgtype": "text",
		"text": map[string]string{
			"content": msg,
		},
	}

	resp, err := http.Post(cfg.URL, "application/json", bytes.NewBuffer(mustJSON(payload)))
	if err != nil {
		return fmt.Errorf("%s webhook post failed (ID: %d): %w", n.Type, n.Id, err)
	}
	defer func() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s API error (ID: %d): %s - %s", n.Type, n.Id, resp.Status, string(body))
	}

	klog.Infof("✅ successfully sent %s notification (ID: %d)", n.Type, n.Id)
	return nil
}

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		klog.Errorf("failed to marshal JSON: %v", err)
		return []byte("{}")
	}
	return b
}

func (s *ServerController) SendRegisterNotify(ctx context.Context, req *types.SendNotificationRequest) error {
	notifies, err := s.factory.Notify().List(ctx, db.WithRole(req.Role), db.WithEnable(1))
	if err != nil {
		return fmt.Errorf("failed to query register notify list: %w", err)
	}

	for _, n := range notifies {
		var cfg struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(n.PushCfg), &cfg); err != nil {
			klog.Errorf("failed to parse config (ID: %d): %v", n.Id, err)
			continue
		}
		if cfg.URL == "" {
			klog.Errorf("invalid config (ID: %d): empty URL", n.Id)
			continue
		}

		msg := fmt.Sprintf(
			"注册通知\n用户名: %s\n时间: %s\nEmail: %s",
			req.UserName, time.Now().Format("2006-01-02 15:04:05"), "",
		)

		client := util.NewHttpClient(5*time.Second, cfg.URL)
		err = client.Post(cfg.URL, nil,
			PushMessage{
				Text:    map[string]string{"content": msg},
				Msgtype: "text",
			},
			map[string]string{"Content-Type": "application/json"},
		)
		if err != nil {
			klog.Errorf("notify(%s) 推送失败: %v", n.Name, err)
			continue
		}
		klog.Infof("notify(%s) 推送成功", n.Name)
	}

	return nil
}

func (s *ServerController) ListNotifies(ctx context.Context, listOption types.ListOptions) ([]model.Notification, error) {
	ns, err := s.factory.Notify().List(ctx)
	if err != nil {
		return nil, err
	}

	return ns, nil
}
