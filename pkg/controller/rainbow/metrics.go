package rainbow

import (
	"context"
	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/types"
	"k8s.io/klog/v2"
)

func (s *ServerController) ListMetrics(ctx context.Context, listOption types.ListOptions) (interface{}, error) {
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
	}

	var err error
	pageResult.Total, err = s.factory.Metrics().CountMetrics(ctx, opts...)
	if err != nil {
		klog.Errorf("获取Metric总数失败 %v", err)
		pageResult.Message = err.Error()
	}
	offset := (listOption.Page - 1) * listOption.Limit
	opts = append(opts, []db.Options{
		db.WithModifyOrderByDesc(),
		db.WithOffset(offset),
		db.WithLimit(listOption.Limit),
	}...)
	pageResult.Items, err = s.factory.Metrics().List(ctx, opts...)
	if err != nil {
		klog.Errorf("获取Metric列表失败 %v", err)
		pageResult.Message = err.Error()
		return pageResult, err
	}

	return pageResult, nil
}
