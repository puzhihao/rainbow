package rainbow

import (
	"context"
	"fmt"
	"k8s.io/klog/v2"
	"time"

	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/db/model/rainbow"
	"github.com/caoyingjunz/rainbow/pkg/types"
	"github.com/caoyingjunz/rainbow/pkg/util"
)

type PushMessage struct {
	Text    map[string]string `json:"text"`
	Msgtype string            `json:"msgtype"`
}

// TODO: 根据不同的类型，检查对应配置是否缺失
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

func (s *ServerController) GetNotify(ctx context.Context, notifyId int64) (*model.Notification, error) {
	return s.factory.Notify().Get(ctx, notifyId)
}

func (s *ServerController) DeleteNotify(ctx context.Context, notifyId int64) error {
	return s.factory.Notify().Delete(ctx, notifyId)
}

func (s *ServerController) makeSendTpl(req *types.SendNotificationRequest) string {
	// 1. 构造推送内容模板
	tpl := fmt.Sprintf("%s\n用户名: %s\n时间: %v\nEmail: %s", req.ShortDesc, req.UserName, time.Now().Format("2006-01-02 15:04:05"), req.Email)
	if req.Role == types.UserNotifyRole {
		tpl = fmt.Sprintf("PixiuHub 同步通知\n时间: %v\n: %s", time.Now().Format("2006-01-02 15:04:05"), req.Content)
		if req.DryRun {
			tpl = fmt.Sprintf("%s\nHello PixiuHub", req.ShortDesc)
		}
	}
	return tpl
}

func (s *ServerController) SendNotify(ctx context.Context, req *types.SendNotificationRequest) error {
	opts := []db.Options{db.WithRole(req.Role), db.WithEnable(1)}
	if req.Role == types.UserNotifyRole {
		opts = append(opts, db.WithUser(req.UserId))
	}
	notifies, err := s.factory.Notify().List(ctx, opts...)
	if err != nil {
		klog.Errorf("获取 notify 失败 %v", err)
		return err
	}
	if len(notifies) == 0 {
		klog.Warningf("未发现(%s)已开启的通知通道，忽略本次通知", req.UserId)
		return nil
	}

	for _, notify := range notifies {
		var pushCfg types.PushConfig
		if err = pushCfg.Unmarshal(notify.PushCfg); err != nil {
			return err
		}

		switch notify.Type {
		case types.DingtalkNotifyType, types.QiWeiNotifyType: // 企微和钉钉的推送格式相同
			// 1. 构造请求地址
			url := pushCfg.Dingtalk.URL
			if notify.Type == types.QiWeiNotifyType {
				url = pushCfg.QiWei.URL
			}
			// 2. 发送推送请求
			if err = s.send(ctx, url, PushMessage{Text: map[string]string{"content": s.makeSendTpl(req)}, Msgtype: "text"}); err != nil {
				klog.Errorf("notify(%s) 推送失败 %v", notify.Name, err)
				return err
			}
		default:
			return fmt.Errorf("unsupported message type %s", notify.Type)
		}

		klog.Infof("notify(%s) 推送成功", notify.Name)
	}
	return nil
}

func (s *ServerController) send(ctx context.Context, url string, val interface{}) error {
	http2 := util.NewHttpClient(5*time.Second, url)
	if err := http2.Post(url, nil, val, map[string]string{"Content-Type": "application/json"}); err != nil {
		klog.Errorf("发送请求 %s 失败 %v", url, err)
		return err
	}

	return nil
}

func (s *ServerController) ListNotifies(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
	// 设置默认值
	listOption.SetDefaultPageOption()
	pageResult := types.PageResult{
		PageRequest: types.PageRequest{
			Page:  listOption.Page,
			Limit: listOption.Limit,
		},
	}

	opts := []db.Options{ // 先写条件，再写排序，再偏移，再设置每页数量
		db.WithUser(listOption.UserId),
	}
	var err error
	pageResult.Total, err = s.factory.Notify().Count(ctx, opts...)
	if err != nil {
		klog.Errorf("获取镜像总数失败 %v", err)
		pageResult.Message = err.Error()
	}

	offset := (listOption.Page - 1) * listOption.Limit
	opts = append(opts, []db.Options{
		db.WithModifyOrderByDesc(),
		db.WithOffset(offset),
		db.WithLimit(listOption.Limit),
	}...)

	pageResult.Items, err = s.factory.Notify().List(ctx, opts...)
	if err != nil {
		klog.Errorf("获取镜像列表失败 %v", err)
		pageResult.Message = err.Error()
		return pageResult, err
	}

	return pageResult, nil
}
