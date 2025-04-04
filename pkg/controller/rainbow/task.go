package rainbow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/types"
	"github.com/caoyingjunz/rainbow/pkg/util"
	"github.com/caoyingjunz/rainbow/pkg/util/errors"
)

func (s *ServerController) CreateTask(ctx context.Context, req *types.CreateTaskRequest) error {
	object, err := s.factory.Task().Create(ctx, &model.Task{
		Name:              req.Name,
		UserId:            req.UserId,
		UserName:          req.UserName,
		RegisterId:        req.RegisterId,
		AgentName:         req.AgentName,
		Mode:              req.Mode,
		Status:            "等待执行",
		Type:              req.Type,
		KubernetesVersion: req.KubernetesVersion,
		Driver:            req.Driver,
	})
	if err != nil {
		return err
	}
	if req.Type == 1 {
		return nil
	}

	taskId := object.Id
	if err = s.CreateImageWithTag(ctx, taskId, req); err != nil {
		_ = s.DeleteTaskWithImages(ctx, taskId)
		return fmt.Errorf("failed to create tasks images %v", err)

	}
	return nil
}

func (s *ServerController) CreateImageWithTag(ctx context.Context, taskId int64, req *types.CreateTaskRequest) error {
	if len(req.Images) == 0 {
		return nil
	}

	imageMap := make(map[string][]string)
	for _, i := range util.TrimAndFilter(req.Images) {
		path, tag, err := ParseImageItem(i)
		if err != nil {
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

	for path, tags := range imageMap {
		var imageId int64
		oldImage, err := s.factory.Image().GetByPath(ctx, path, db.WithUser(req.UserId))
		if err != nil {
			// 镜像不存在，则先创建镜像
			if errors.IsNotFound(err) {
				parts2 := strings.Split(path, "/")
				name := parts2[len(parts2)-1]
				if len(name) == 0 {
					return fmt.Errorf("不合规镜像名称 %s", path)
				}

				newImage, err := s.factory.Image().Create(ctx, &model.Image{
					TaskId:     taskId,
					TaskName:   req.Name,
					UserId:     req.UserId,
					UserName:   req.UserName,
					RegisterId: req.RegisterId,
					GmtDeleted: time.Now(),
					Name:       name,
					Status:     "同步准备中",
					Path:       path,
				})
				if err != nil {
					return err
				}
				imageId = newImage.Id
			} else {
				return err
			}
		} else {
			imageId = oldImage.Id
		}

		for _, tag := range tags {
			_, err = s.factory.Image().GetTagByImage(ctx, imageId, tag)
			if err != nil {
				if errors.IsNotFound(err) {
					_, err = s.factory.Image().CreateTag(ctx, &model.Tag{
						Path:    path,
						ImageId: imageId,
						Name:    tag,
					})
					if err != nil {
						return err
					}
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
			TaskId: req.Id,
			Name:   i,
			Status: "同步准备中",
		})
	}
	if err = s.factory.Image().CreateInBatch(ctx, images); err != nil {
		return fmt.Errorf("failed to create tasks images %v", err)
	}

	return nil
}

func (s *ServerController) ListTasks(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
	return s.factory.Task().List(ctx, db.WithUser(listOption.UserId), db.WithNameLike(listOption.NameSelector), db.WithOrderByDesc())
}

func (s *ServerController) UpdateTaskStatus(ctx context.Context, req *types.UpdateTaskStatusRequest) error {
	return s.factory.Task().UpdateDirectly(ctx, req.TaskId, map[string]interface{}{"status": req.Status, "message": req.Message, "process": req.Process})
}

func (s *ServerController) DeleteTask(ctx context.Context, taskId int64) error {
	return s.DeleteTaskWithImages(ctx, taskId)
}

func (s *ServerController) DeleteTaskWithImages(ctx context.Context, taskId int64) error {
	_ = s.factory.Image().SoftDeleteInBatch(ctx, taskId)
	_ = s.factory.Task().Delete(ctx, taskId)
	return nil
}
