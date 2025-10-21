package rainbow

import (
	"context"
	"fmt"
	"strings"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/db/model/rainbow"
	"github.com/caoyingjunz/rainbow/pkg/types"
	"github.com/caoyingjunz/rainbow/pkg/util"
	"github.com/caoyingjunz/rainbow/pkg/util/errors"
	"github.com/caoyingjunz/rainbow/pkg/util/uuid"
)

const (
	TaskWaitStatus  = "调度中"
	HuaweiNamespace = "pixiu-public"
)

func (s *ServerController) preCreateTask(ctx context.Context, req *types.CreateTaskRequest) error {
	if req.Type == 1 {
		if !strings.HasPrefix(req.KubernetesVersion, "v1.") {
			return fmt.Errorf("invaild kubernetes version (%s)", req.KubernetesVersion)
		}
	} else {
		var errs []error
		// TODO: 其他不合规检查
		for _, image := range req.Images {
			if strings.Contains(image, "\"") { // 分割镜像名称和版本
				errs = append(errs, fmt.Errorf("invaild image(%s)", image))
				parts := strings.Split(image, ":")
				if len(parts) != 2 {
					errs = append(errs, fmt.Errorf("invalid image format, should be 'name:tag' (%s)", image))
				}
			}
		}
		return utilerrors.NewAggregate(errs)
	}

	return nil
}

const (
	defaultNamespace = "emptyNamespace"
	defaultArch      = "linux/amd64"
)

func (s *ServerController) CreateTask(ctx context.Context, req *types.CreateTaskRequest) error {
	if err := s.preCreateTask(ctx, req); err != nil {
		klog.Errorf("创建任务前置检查未通过 %v", err)
		return err
	}

	// 填充任务名称
	if len(strings.TrimSpace(req.Name)) == 0 {
		req.Name = uuid.NewRandName("", 8)
	}
	// 初始化仓库
	if req.RegisterId == 0 {
		req.RegisterId = *RegistryId
	}

	if len(req.Namespace) == 0 {
		req.Namespace = strings.ToLower(req.UserName)
	}
	if req.Namespace == defaultNamespace {
		req.Namespace = ""
	}
	if len(req.Architecture) == 0 {
		req.Architecture = defaultArch
	}
	if len(req.Driver) == 0 {
		req.Driver = "skopeo"
	}

	// 如果是k8s类型的镜像，则由 plugin 回调创建
	// 0：直接指定镜像列表 1: 指定 kubernetes 版本
	switch req.Type {
	case 0:
		object, err := s.factory.Task().Create(ctx, &model.Task{
			Name:              req.Name,
			UserId:            req.UserId,
			UserName:          req.UserName,
			RegisterId:        req.RegisterId,
			AgentName:         req.AgentName,
			Mode:              req.Mode,
			Status:            TaskWaitStatus,
			Type:              req.Type,
			KubernetesVersion: req.KubernetesVersion,
			Driver:            req.Driver,
			Namespace:         req.Namespace,
			IsPublic:          req.PublicImage,
			Logo:              req.Logo,
			IsOfficial:        req.IsOfficial,
			Architecture:      req.Architecture, // 通用镜像架构，会被镜像自身的架构覆盖
			OwnerRef:          req.OwnerRef,
			SubscribeId:       req.SubscribeId,
		})
		if err != nil {
			return err
		}
		taskId := object.Id
		s.CreateTaskMessages(ctx, taskId, "同步已启动", "数据校验中，预计等待 1 分钟")

		if err = s.CreateImageWithTag(ctx, taskId, req); err != nil {
			s.CreateTaskMessages(ctx, taskId, fmt.Sprintf("创建镜像和版本失败 %v", err))
			_ = s.DeleteTaskWithImages(ctx, taskId)
			return err
		}
	case 1:
		kubernetesVersions := strings.Split(req.KubernetesVersion, ",")
		for _, kv := range kubernetesVersions {
			subName := req.Name + "-" + kv
			object, err := s.factory.Task().Create(ctx, &model.Task{
				Name:              subName,
				UserId:            req.UserId,
				UserName:          req.UserName,
				RegisterId:        req.RegisterId,
				AgentName:         req.AgentName,
				Mode:              req.Mode,
				Status:            TaskWaitStatus,
				Type:              req.Type,
				KubernetesVersion: kv,
				Driver:            req.Driver,
				Namespace:         req.Namespace,
				IsPublic:          req.PublicImage,
				Logo:              req.Logo,
				IsOfficial:        req.IsOfficial,
				Architecture:      req.Architecture,
				OwnerRef:          req.OwnerRef,
				SubscribeId:       req.SubscribeId,
			})
			if err != nil {
				return err
			}

			s.CreateTaskMessages(ctx, object.Id, "同步已启动", "数据校验中，预计等待 1 分钟")
			klog.Infof("(%s) 的子任务(%s)创建成功，其类型为 kubernetes，镜像由 plugin 回调创建", req.Name, subName)
		}
	}

	return nil
}

// CreateTaskMessages 批量创建同步消息
func (s *ServerController) CreateTaskMessages(ctx context.Context, taskId int64, messages ...string) {
	for _, msg := range messages {
		if err := s.factory.Task().CreateTaskMessage(ctx, &model.TaskMessage{TaskId: taskId, Message: msg}); err != nil {
			klog.Errorf("记录 %s 失败 %v", msg, err)
		}
	}
}

func (s *ServerController) CreateImageWithTag(ctx context.Context, taskId int64, req *types.CreateTaskRequest) error {
	klog.Infof("使用镜像仓库(%d)", req.RegisterId)
	reg, err := s.factory.Registry().Get(ctx, req.RegisterId)
	if err != nil {
		return fmt.Errorf("获取仓库(%d)失败 %v", req.RegisterId, err)
	}

	imageMap := make(map[string][]string)
	for _, i := range util.TrimAndFilter(req.Images) {
		path, tag, err := ParseImageItem(i)
		if err != nil {
			klog.Errorf("无法解析镜像 %s %v", i, err)
			return fmt.Errorf("failed to parse image %v", err)
		}

		old, ok := imageMap[path]
		if ok {
			old = append(old, tag)
			imageMap[path] = old
		} else {
			imageMap[path] = []string{tag}
		}
	}

	namespace := req.Namespace
	for path, tags := range imageMap {
		var imageId int64
		parts2 := strings.Split(path, "/")
		name := parts2[len(parts2)-1]
		if len(name) == 0 {
			return fmt.Errorf("不合规镜像名称 %s", path)
		}
		if req.RegisterId == *RegistryId {
			if len(namespace) != 0 {
				name = namespace + "/" + name
			}
		}

		mirror := reg.Repository + "/" + reg.Namespace + "/" + name

		oldImage, err := s.factory.Image().GetByPath(ctx, path, mirror, db.WithUser(req.UserId))
		if err != nil {
			// 镜像不存在，则先创建镜像
			if errors.IsNotFound(err) {
				newImage, err := s.factory.Image().Create(ctx, &model.Image{
					UserId:     req.UserId,
					UserName:   req.UserName,
					RegisterId: req.RegisterId,
					Namespace:  namespace,
					Logo:       req.Logo,
					Name:       name,
					Path:       path,
					Mirror:     mirror,
					IsPublic:   req.PublicImage,
					IsOfficial: req.IsOfficial,
					IsLocked:   true,
				})
				if err != nil {
					klog.Errorf("创建镜像(%s)失败: %v", path, err)
					return err
				}
				imageId = newImage.Id
			} else {
				return err
			}
		} else {
			klog.Infof("镜像(%s)已存在，复用", path)
			imageId = oldImage.Id
		}

		// 版本需要和任务关联
		for _, tag := range tags {
			oldTag, tagErr := s.factory.Image().GetTagWithArch(ctx, imageId, tag, req.Architecture, false)
			if tagErr != nil {
				// 非不存在报错，则直接返回异常
				if !errors.IsNotFound(tagErr) {
					klog.Errorf("获取镜像(%d)的版本(%s)失败: %v", imageId, tag, tagErr)
					return tagErr
				}

				// tag 不存在则创建
				if _, err = s.factory.Image().CreateTag(ctx, &model.Tag{
					Path:         path,
					Mirror:       mirror,
					ImageId:      imageId,
					TaskIds:      fmt.Sprintf("%d", taskId),
					Name:         tag,
					Status:       types.SyncImageInitializing,
					Architecture: req.Architecture,
				}); err != nil {
					klog.Errorf("创建镜像(%s)的版本(%s)失败 %v", path, tag, err)
					return err
				}
			} else {
				// 已经存在则写入新关联的 taskId
				newTaskIds := strings.Join([]string{oldTag.TaskIds, fmt.Sprintf("%d", taskId)}, ",")
				update := map[string]interface{}{"task_ids": newTaskIds}
				// 必要时更新 mirror
				// 早期创建的 tag 不存在 mirror 字段，会在其他人任务推送时展示 null
				if mirror != oldTag.Mirror {
					update["mirror"] = mirror
				}
				if err = s.factory.Image().UpdateTag(ctx, imageId, tag, update); err != nil {
					klog.Errorf("更新镜像(%s)的版本(%s)任务Id失败 %v", path, tag, err)
					return err
				}
			}
		}
	}

	return nil
}

func ParseImageItem(image string) (string, string, error) {
	parts := strings.Split(image, ":")
	if len(parts) > 2 || len(parts) == 0 {
		return "", "", fmt.Errorf("不合规镜像名称 %s", image)
	}

	path := parts[0]
	tag := "latest"
	if len(parts) == 2 {
		tag = parts[1]
	}

	// 如果镜像是以 docker.io 开关，则去除 docker.io
	if strings.HasPrefix(path, "docker.io/") {
		path = strings.Replace(path, "docker.io/", "", 1)
	}

	return path, tag, nil
}

func (s *ServerController) UpdateTask(ctx context.Context, req *types.UpdateTaskRequest) error {
	if err := s.factory.Task().Update(ctx, req.Id, req.ResourceVersion, map[string]interface{}{
		"register_id": req.RegisterId,
		"mode":        req.Mode,
	}); err != nil {
		return err
	}

	old, err := s.factory.Image().ListWithTask(ctx, req.Id)
	if err != nil {
		return err
	}
	var oldImages []string
	for _, o := range old {
		oldImages = append(oldImages, o.Name)
	}
	oldImageMap := sets.NewString(oldImages...)

	var addImages []string
	for _, n := range req.Images {
		if oldImageMap.Has(n) {
			continue
		}
		addImages = append(addImages, n)
	}
	var images []model.Image
	trimAddImages := util.TrimAndFilter(req.Images)
	for _, i := range trimAddImages {
		images = append(images, model.Image{
			Name: i,
		})
	}
	if err = s.factory.Image().CreateInBatch(ctx, images); err != nil {
		return fmt.Errorf("failed to create tasks images %v", err)
	}

	return nil
}

func (s *ServerController) ListSubscribes(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
	listOption.SetDefaultPageOption()

	pageResult := types.PageResult{
		PageRequest: types.PageRequest{
			Page:  listOption.Page,
			Limit: listOption.Limit,
		},
	}
	opts := []db.Options{
		db.WithUser(listOption.UserId),
		db.WithPathLike(listOption.NameSelector),
		db.WithNamespace(listOption.Namespace),
	}
	var err error
	pageResult.Total, err = s.factory.Task().CountSubscribe(ctx, opts...)
	if err != nil {
		klog.Errorf("获取订阅总数失败 %v", err)
		pageResult.Message = err.Error()
	}
	offset := (listOption.Page - 1) * listOption.Limit
	opts = append(opts, []db.Options{
		db.WithModifyOrderByDesc(),
		db.WithOffset(offset),
		db.WithLimit(listOption.Limit),
	}...)
	pageResult.Items, err = s.factory.Task().ListSubscribes(ctx, opts...)
	if err != nil {
		klog.Errorf("获取订阅列表失败 %v", err)
		pageResult.Message = err.Error()
		return pageResult, err
	}

	return pageResult, nil
}

func (s *ServerController) ListTasks(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
	// 初始化分页属性
	listOption.SetDefaultPageOption()

	pageResult := types.PageResult{
		PageRequest: types.PageRequest{
			Page:  listOption.Page,
			Limit: listOption.Limit,
		},
	}
	opts := []db.Options{
		db.WithUser(listOption.UserId),
		db.WithNameLike(listOption.NameSelector),
		db.WithNamespace(listOption.Namespace),
		db.WithAgent(listOption.Agent),
		db.WithRef(listOption.OwnerRef),
		db.WithSubscribe(listOption.SubscribeId),
	}

	var err error
	pageResult.Total, err = s.factory.Task().Count(ctx, opts...)
	if err != nil {
		klog.Errorf("获取任务总数失败 %v", err)
		pageResult.Message = err.Error()
	}
	offset := (listOption.Page - 1) * listOption.Limit
	opts = append(opts, []db.Options{
		db.WithModifyOrderByDesc(),
		db.WithOffset(offset),
		db.WithLimit(listOption.Limit),
	}...)
	pageResult.Items, err = s.factory.Task().List(ctx, opts...)
	if err != nil {
		klog.Errorf("获取推送任务列表失败 %v", err)
		pageResult.Message = err.Error()
		return pageResult, err
	}

	return pageResult, nil
}

func (s *ServerController) UpdateTaskStatus(ctx context.Context, req *types.UpdateTaskStatusRequest) error {
	return s.factory.Task().UpdateDirectly(ctx, req.TaskId, map[string]interface{}{"status": req.Status, "message": req.Message, "process": req.Process})
}

func (s *ServerController) DeleteTask(ctx context.Context, taskId int64) error {
	return s.factory.Task().Delete(ctx, taskId)
}

func (s *ServerController) DeleteTaskWithImages(ctx context.Context, taskId int64) error {
	_ = s.factory.Task().Delete(ctx, taskId)
	return nil
}

func (s *ServerController) GetTask(ctx context.Context, taskId int64) (interface{}, error) {
	return s.factory.Task().Get(ctx, taskId)
}

func (s *ServerController) ReRunTask(ctx context.Context, req *types.UpdateTaskRequest) error {
	updates := map[string]interface{}{
		"agent_name":      "",
		"status":          TaskWaitStatus,
		"process":         0,
		"message":         "触发重新执行",
		"only_push_error": req.OnlyPushError,
	}
	if err := s.factory.Task().Update(ctx, req.Id, req.ResourceVersion, updates); err != nil {
		klog.Errorf("重新执行任务 %d 失败 %v", req.Id, err)
		return err
	}

	// 全量重新推送时，重置任务过程信息
	if !req.OnlyPushError {
		if err := s.factory.Task().DeleteTaskMessages(ctx, req.Id); err != nil {
			klog.Errorf("清理任务(%d)过程信息失败 %v", req.Id, err)
		}
	}
	return nil
}

func (s *ServerController) ListTaskImages(ctx context.Context, taskId int64, listOption types.ListOptions) (interface{}, error) {
	return s.factory.Image().ListTags(ctx, db.WithTaskLike(taskId), db.WithNameLike(listOption.NameSelector))
}

func (s *ServerController) CreateTaskMessage(ctx context.Context, req types.CreateTaskMessageRequest) error {
	return s.factory.Task().CreateTaskMessage(ctx, &model.TaskMessage{
		Message: req.Message,
		TaskId:  req.Id,
	})
}

func (s *ServerController) ListTaskMessages(ctx context.Context, taskId int64) (interface{}, error) {
	return s.factory.Task().ListTaskMessages(ctx, db.WithTask(taskId))
}

func (s *ServerController) ListTasksByIds(ctx context.Context, ids []int64) (interface{}, error) {
	return s.factory.Task().List(ctx, db.WithIDIn(ids...))
}

func (s *ServerController) DeleteTasksByIds(ctx context.Context, ids []int64) error {
	return s.factory.Task().DeleteInBatch(ctx, ids)
}

func (s *ServerController) preCreateSubscribe(ctx context.Context, req *types.CreateSubscribeRequest) error {
	if req.Size > 100 {
		return fmt.Errorf("订阅镜像版本数超过阈值 100")
	}

	old, err := s.factory.Task().ListSubscribes(ctx, db.WithPath(req.Path), db.WithUser(req.UserId))
	if err != nil {
		return fmt.Errorf("创建前置检查时，获取订阅列表失败 %v", err)
	}
	if len(old) != 0 {
		return fmt.Errorf("用户(%s)已存在订阅镜像(%s)，无法重复创建", req.UserName, req.Path)
	}

	return nil
}

func (s *ServerController) CreateSubscribe(ctx context.Context, req *types.CreateSubscribeRequest) error {
	if err := s.preCreateSubscribe(ctx, req); err != nil {
		return err
	}

	parts2 := strings.Split(req.Path, "/")
	srcPath := parts2[len(parts2)-1]
	if len(srcPath) == 0 {
		return fmt.Errorf("不合规镜像名称 %s", req.Path)
	}

	ns := req.Namespace
	if len(ns) == 0 {
		ns = strings.ToLower(req.UserName)
	}
	if ns == defaultNamespace {
		ns = ""
	}
	if len(ns) != 0 {
		srcPath = ns + "/" + srcPath
	}

	// 初始化镜像来源
	if len(req.ImageFrom) == 0 {
		req.ImageFrom = types.ImageHubDocker
	}

	rawPath := req.Path
	// 默认dockerhub不指定 ns时，会添加默认值
	if req.ImageFrom == types.ImageHubDocker {
		parts := strings.Split(rawPath, "/")
		if len(parts) == 1 {
			rawPath = "library" + "/" + rawPath
		}
	}

	req.Policy = strings.TrimSpace(req.Policy)
	if len(req.Policy) == 0 {
		req.Policy = "latest"
	}

	return s.factory.Task().CreateSubscribe(ctx, &model.Subscribe{
		UserModel: rainbow.UserModel{
			UserId:   req.UserId,
			UserName: req.UserName,
		},
		Namespace: req.Namespace,
		Path:      req.Path,
		RawPath:   rawPath,
		SrcPath:   srcPath,
		Enable:    req.Enable,   // 是否启动订阅
		Size:      req.Size,     // 最多同步多少个版本
		Interval:  req.Interval, // 多久执行一次
		ImageFrom: req.ImageFrom,
		Policy:    req.Policy,
		Arch:      req.Arch,
		Rewrite:   req.Rewrite,
	})
}

func (s *ServerController) UpdateSubscribe(ctx context.Context, req *types.UpdateSubscribeRequest) error {
	update := map[string]interface{}{
		"size":       req.Size,
		"interval":   req.Interval,
		"image_from": req.ImageFrom,
		"policy":     req.Policy,
		"arch":       req.Arch,
		"rewrite":    req.Rewrite,
		"namespace":  req.Namespace,
	}

	enable := req.Enable
	old, err := s.factory.Task().GetSubscribe(ctx, req.Id)
	if err == nil {
		if enable != old.Enable {
			update["enable"] = enable

			msg := ""
			// 原先是关闭，最新开启，则刷新 sub message 为开启
			if !old.Enable && enable {
				msg = fmt.Sprintf("手动启动制品订阅")
			}
			if old.Enable && !enable {
				msg = fmt.Sprintf("手动关闭制品订阅")
			}
			s.CreateSubscribeMessageWithLog(ctx, *old, msg)
		}
	}

	if err = s.factory.Task().UpdateSubscribe(ctx, req.Id, req.ResourceVersion, update); err != nil {
		return err
	}
	return nil
}

// DeleteSubscribe 删除订阅
// 1. 删除订阅
// 2. 删除订阅记录
// 3. 删除订阅关联的同步任务
func (s *ServerController) DeleteSubscribe(ctx context.Context, subId int64) error {
	if err := s.factory.Task().DeleteSubscribe(ctx, subId); err != nil {
		return err
	}
	if err := s.factory.Task().DeleteSubscribeAllMessage(ctx, subId); err != nil {
		return err
	}
	if err := s.factory.Task().DeleteBySubscribe(ctx, subId); err != nil {
		return err
	}

	return nil
}

func (s *ServerController) preCreateAgent(ctx context.Context, req *types.CreateAgentRequest) error {
	if len(req.AgentName) == 0 {
		return fmt.Errorf("agent 名称不能为空")
	}
	if len(req.GithubUser) == 0 {
		return fmt.Errorf("github 用户名不能为空")
	}
	if len(req.GithubToken) == 0 {
		return fmt.Errorf("github token不能为空")
	}
	if len(req.GithubEmail) == 0 {
		return fmt.Errorf("github email不能为空")
	}

	// 检查agent是否存在
	_, err := s.factory.Agent().GetByName(ctx, req.AgentName)
	if err == nil {
		return fmt.Errorf("agent名称 %s 已存在", req.AgentName)
	}

	return nil
}

func (s *ServerController) CreateAgent(ctx context.Context, req *types.CreateAgentRequest) error {
	if err := s.preCreateAgent(ctx, req); err != nil {
		klog.Infof("创建agent前置检查失败: %v", err)
		return err
	}

	if req.HealthzPort == 0 {
		// 未做去重，暂时人工确认
		req.HealthzPort = util.GenRandInt(10086, 10186)
	}
	if len(req.GithubRepository) == 0 {
		req.GithubRepository = fmt.Sprintf("https://github.com/%s/plugin.git", req.GithubUser)
	}
	// 创建新的agent记录
	agent := &model.Agent{
		Name:             req.AgentName,
		GithubUser:       req.GithubUser,
		GithubToken:      req.GithubToken,
		GithubRepository: req.GithubRepository,
		GithubEmail:      req.GithubEmail,
		HealthzPort:      req.HealthzPort,
		Type:             req.Type,
		RainbowdName:     req.RainbowdName,
		Status:           model.UnStartType,
	}
	if _, err := s.factory.Agent().Create(ctx, agent); err != nil {
		return fmt.Errorf("创建agent失败: %v", err)
	}

	return nil
}

// DeleteAgent 删除已注册的agent信息
func (s *ServerController) DeleteAgent(ctx context.Context, agentId int64) error {
	// TODO 检查是否有正在运行的任务关联该agent
	// 执行删除操作
	old, err := s.factory.Agent().Get(ctx, agentId)
	if err != nil {
		return err
	}
	if err = s.factory.Agent().Update(ctx, agentId, old.ResourceVersion, map[string]interface{}{
		"status": model.DeletingAgentType,
	}); err != nil {
		return fmt.Errorf("删除agent失败: %v", err)
	}

	// 清理缓存
	klog.V(0).Infof("清理 agent(%s) 缓存", old.Name)
	s.lock.Lock()
	defer s.lock.Unlock()
	if RpcClients != nil {
		delete(RpcClients, old.Name)
	}

	return nil
}

func (s *ServerController) ListRainbowds(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
	return s.factory.Rainbowd().List(ctx)
}
