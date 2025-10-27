package rainbow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/db/model/rainbow"
	"github.com/caoyingjunz/rainbow/pkg/types"
)

type PushMessage struct {
	Text    map[string]string `json:"text"`
	Msgtype string            `json:"msgtype"`
}

func (s *ServerController) CreateNotify(ctx context.Context, req *types.CreateNotificationRequest) error {
	_, err := s.factory.Notify().Create(ctx, &model.Notification{
		Name:   req.Name,
		Role:   req.Role,
		Enable: req.Enable,
		Type:   req.Type,
		UserModel: rainbow.UserModel{
			UserId:   req.UserId,
			UserName: req.UserName,
		},
		PushCfg:   req.PushCfg,
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

	for _, n := range list {
		switch n.Type {
		case "wecom":
			err := s.SendWeComNotify(ctx, &n, req.Content)
			if err != nil {
				return err
			}
		case "dingding":
			// TODO
		case "email":
			// TODO
		default:
			// TODO
		}
	return nil
}

func (s *ServerController) SendWeComNotify(ctx context.Context, req *model.Notification, msg string) error {

	type WeComConfig struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}

	var wecomCfg WeComConfig
	if err := json.Unmarshal([]byte(req.PushCfg), &wecomCfg); err != nil {
		klog.Errorf("failed to parse WeCom config (ID: %d): %v", req.Id, err)
	}

	if wecomCfg.URL == "" || wecomCfg.Token == "" {
		klog.Errorf("invalid WeCom config (ID: %d): empty URL or token", req.Id)
	}

	apiURL := fmt.Sprintf("%s?key=%s", wecomCfg.URL, wecomCfg.Token)
	payload := map[string]interface{}{
		"msgtype": "text",
		"text": map[string]string{
			"content": msg, // 使用请求中的实际内容
		},
	}

	resp, err := http.Post(apiURL, "application/json", bytes.NewBuffer(marshal(payload)))
	if err != nil {
		klog.Errorf("failed to send to WeCom (ID: %d): %v", req.Id, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			klog.Warningf("failed to close response body (ID: %d): %v", req.Id, err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		klog.Errorf("WeCom API error (ID: %d): %s - %s", req.Id, resp.Status, string(body))

	}
	klog.Infof("successfully sent notification via config (ID: %d)", req.Id)
	return nil
}

func marshal(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		klog.Errorf("failed to marshal JSON: %v", err)
		return []byte("{}")
	}
	return b
}

//func (s *ServerController) SendRegisterNotify(ctx context.Context, req *types.SendNotificationRequest) error {
//	notifies, err := s.factory.Notify().List(ctx, db.WithRole(req.Role), db.WithEnable(1))
//	if err != nil {
//		klog.Errorf("获取 notify 失败 %v", err)
//		return err
//	}
//	for _, notify := range notifies {
//		http := util.NewHttpClient(5*time.Second, notify.Url)
//		msg := fmt.Sprintf("注册通知\n用户名: %s\n时间: %v\nEmail: %s", req.UserName, time.Now().Format("2006-01-02 15:04:05"), req.Email)
//		if err = http.Post(notify.Url, nil,
//			PushMessage{Text: map[string]string{"content": msg}, Msgtype: "text"}, map[string]string{"Content-Type": "application/json"}); err != nil {
//			klog.Errorf("notify(%s) 推送失败 %v", notify.Name, err)
//			continue
//		}
//		klog.Errorf("notify(%s) 推送成功", notify.Name)
//	}
//
//	return nil
//}
