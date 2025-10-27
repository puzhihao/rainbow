package rainbowd

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/apache/rocketmq-client-go/v2/consumer"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	"github.com/caoyingjunz/pixiulib/exec"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"

	rainbowconfig "github.com/caoyingjunz/rainbow/cmd/app/config"
	"github.com/caoyingjunz/rainbow/pkg/db"
	"github.com/caoyingjunz/rainbow/pkg/db/model"
	"github.com/caoyingjunz/rainbow/pkg/util"
)

type RainbowdGetter interface {
	Rainbowd() Interface
}

type Interface interface {
	Run(ctx context.Context, workers int) error
	Subscribe(ctx context.Context, msgs ...*primitive.MessageExt) (consumer.ConsumeResult, error)
}

type rainbowdController struct {
	name    string
	factory db.ShareDaoFactory
	cfg     rainbowconfig.Config
	exec    exec.Interface
}

func New(f db.ShareDaoFactory, cfg rainbowconfig.Config) *rainbowdController {
	return &rainbowdController{
		factory: f,
		cfg:     cfg,
		name:    cfg.Rainbowd.Name,
		exec:    exec.New(),
	}
}

func (s *rainbowdController) Subscribe(ctx context.Context, msgs ...*primitive.MessageExt) (consumer.ConsumeResult, error) {
	for _, msg := range msgs {
		klog.V(0).Infof("收到消息: Topic=%s, MessageID=%s, Body=%s", msg.Topic, msg.MsgId, string(msg.Body))
		if err := s.handler(ctx, string(msg.Body)); err != nil {
			klog.Errorf("处理 rainbowd 服务失败 %v", err)
		}
	}
	return consumer.ConsumeSuccess, nil
}

func (s *rainbowdController) handler(ctx context.Context, key string) error {
	agentId, resourceVersion, err := util.KeyFunc(key)
	if err != nil {
		return fmt.Errorf("解析获取到的 key %s 失败 %v", key, err)
	}
	if err = s.sync(ctx, agentId, resourceVersion); err != nil {
		_ = s.factory.Agent().Update(context.TODO(), agentId, resourceVersion, map[string]interface{}{"status": model.ErrorAgentType, "message": err.Error()})
		return fmt.Errorf("同步agent状态失败 %v", err)
	}

	return nil
}

func (s *rainbowdController) Run(ctx context.Context, workers int) error {
	if err := s.RegisterIfNotExist(ctx); err != nil {
		klog.Errorf("register rainbowd failed: %v", err)
		return err
	}

	// TODO
	//go s.startHealthChecker(ctx) // 可用性检查

	return nil
}

func (s *rainbowdController) RegisterIfNotExist(ctx context.Context) error {
	if len(s.name) == 0 {
		return fmt.Errorf("rainbowd name is empty")
	}

	var err error
	_, err = s.factory.Rainbowd().GetByName(ctx, s.name)
	if err == nil {
		return nil
	}
	_, err = s.factory.Rainbowd().Create(ctx, &model.Rainbowd{
		Name:   s.name,
		Status: model.RunAgentType,
	})
	return nil
}

func (s *rainbowdController) startHealthChecker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		agents, err := s.factory.Agent().List(ctx, db.WithRainbowdName(s.name), db.WithStatus(model.RunAgentType))
		if err != nil {
			klog.Error("failed to list my agents %v", err)
			continue
		}
		if len(agents) == 0 {
			continue
		}
		for _, agent := range agents {
			klog.V(1).Infof("agent(%s)即将被检测", agent.Name)
			if err = s.doCheck(agent); err != nil {
				klog.Warningf("健康检查失败，尝试重启恢复 %v", err)
				if err = s.restartAgentContainer(&agent); err != nil {
					klog.Errorf("重启agent(%s)失败%v", agent.Name, err)
				} else {
					klog.Infof("agent(%s) 已通过重启恢复", agent.Name)
				}
			}
		}
	}
}

// 一次失败就直接重启
func (s *rainbowdController) doCheck(agent model.Agent) error {
	cmd := []string{"docker", "exec", agent.Name, "curl", "-s", "-o", "/dev/null", "-w", `"%{http_code}"`, "-X", "POST", fmt.Sprintf("http://127.0.0.1:%d/healthz", 10086)}
	out, err := s.exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to exec %s container %v", string(out), err)
	}

	if strings.Contains(string(out), "200") {
		return nil
	}
	return fmt.Errorf("%s", string(out))
}

// 1. 获取 agent 期望状态 (数据库状态)
// 2. 获取 agent 实际运行状态（容器状态）
// 3. 调整容器状态为数据库状态
func (s *rainbowdController) sync(ctx context.Context, agentId int64, resourceVersion int64) error {
	old, err := s.factory.Agent().Get(ctx, agentId)
	if err != nil {
		klog.Errorf("获取 agent %s 失败", err)
		return err
	}

	klog.V(1).Infof("agent(%d/%d) 即将状态同步, 当前状态是 %s", agentId, resourceVersion, old.Status)
	return s.reconcileAgent(old)
}

func (s *rainbowdController) runAgentContainer(agent *model.Agent) error {
	cmd := []string{"docker", "run", "-d", "--name", agent.Name,
		"-v", fmt.Sprintf("%s:/data", s.cfg.Rainbowd.DataDir+"/"+agent.Name),
		"-v", "/etc/localtime:/etc/localtime:ro",
		"--network", "host", s.cfg.Rainbowd.AgentImage, "/data/agent", "--configFile", "/data/config.yaml"}
	if err := s.runCmd(cmd); err != nil {
		return err
	}

	// 输入 github 的配置
	cmd1 := []string{"docker", "exec", agent.Name, "git", "config", "--global", "user.name", agent.GithubUser}
	if err := s.runCmd(cmd1); err != nil {
		klog.Errorf("执行 git user.name 设置失败 %v", err)
		return err
	}
	cmd2 := []string{"docker", "exec", agent.Name, "git", "config", "--global", "user.email", agent.GithubEmail}
	if err := s.runCmd(cmd2); err != nil {
		klog.Errorf("执行 git user.email 设置失败 %v", err)
		return err
	}

	return nil
}

func (s *rainbowdController) runUpgradeAgent(agent *model.Agent) error {
	containerName := "upgrade-" + agent.Name
	cmd := []string{"docker", "run", "-d", "--name", containerName,
		"-v", fmt.Sprintf("%s:/data", s.cfg.Rainbowd.DataDir+"/"+agent.Name),
		"-v", "/etc/localtime:/etc/localtime:ro",
		"--network", "host", s.cfg.Rainbowd.AgentImage, "sleep", "infinity"}
	if err := s.runCmd(cmd); err != nil {
		return err
	}

	pluginDir := "/data/plugin/"

	cmd1 := []string{"docker", "exec", containerName, "git", "init", pluginDir}
	if err := s.runCmd(cmd1); err != nil {
		return err
	}

	// 输入 github 的配置
	cmd3 := []string{"docker", "exec", containerName, "git", "config", "--global", "user.name", agent.GithubUser}
	if err := s.runCmd(cmd3); err != nil {
		klog.Errorf("执行 git user.name 设置失败 %v", err)
		return err
	}
	cmd2 := []string{"docker", "exec", containerName, "git", "config", "--global", "user.email", agent.GithubEmail}
	if err := s.runCmd(cmd2); err != nil {
		klog.Errorf("执行 git user.email 设置失败 %v", err)
		return err
	}

	// 渲染 .git/config
	gc := struct{ URL string }{URL: fmt.Sprintf("https://%s:%s@github.com/%s/plugin.git", agent.GithubUser, agent.GithubToken, agent.GithubUser)}
	tpl := template.New(agent.Name)
	t := template.Must(tpl.Parse(GitConfig))
	var buf bytes.Buffer
	if err := t.Execute(&buf, gc); err != nil {
		return err
	}

	destDir := filepath.Join(s.cfg.Rainbowd.DataDir, agent.Name)
	if err := ioutil.WriteFile(destDir+"/plugin/.git/config", buf.Bytes(), 0644); err != nil {
		return err
	}
	cmd5 := []string{"docker", "exec", "-w", pluginDir, containerName, "git", "add", "."}
	if err := s.runCmd(cmd5); err != nil {
		klog.Errorf("执行 git add . 失败 %v", err)
		return err
	}
	cmd6 := []string{"docker", "exec", "-w", pluginDir, containerName, "git", "commit", "-m", "init"}
	if err := s.runCmd(cmd6); err != nil {
		klog.Errorf("执行 git commit -m init 失败 %v", err)
		return err
	}
	cmd4 := []string{"docker", "exec", "-w", pluginDir, containerName, "git", "push", "--set-upstream", "origin", "master", "--force"}
	if err := s.runCmd(cmd4); err != nil {
		klog.Errorf("执行 git push --set-upstream origin master --force %v", err)
		return err
	}

	return nil
}

func (s *rainbowdController) restartAgentContainer(agent *model.Agent) error {
	cmd := []string{"docker", "restart", agent.Name}
	return s.runCmd(cmd)
}

func (s *rainbowdController) startAgentContainer(agent *model.Agent) error {
	cmd := []string{"docker", "start", agent.Name}
	return s.runCmd(cmd)
}

func (s *rainbowdController) stopAgentContainer(agent *model.Agent) error {
	cmd := []string{"docker", "stop", agent.Name}
	return s.runCmd(cmd)
}

func (s *rainbowdController) removeAgentContainer(agent *model.Agent) error {
	cmd := []string{"docker", "rm", agent.Name, "-f"}
	if err := s.runCmd(cmd); err != nil {
		return err
	}

	// 清理本地文件
	destDir := filepath.Join(s.cfg.Rainbowd.DataDir, agent.Name)
	klog.V(1).Infof("agent 工作目录(%s) 正在被回收", destDir)
	util.RemoveFile(destDir)
	return nil
}

func (s *rainbowdController) runCmd(cmd []string) error {
	if len(cmd) < 2 {
		return fmt.Errorf("invaild cmd %v", cmd)
	}

	out, err := s.exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to exec %s container %v", string(out), err)
	}
	return nil
}

// reconcile agent
func (s *rainbowdController) reconcileAgent(agent *model.Agent) error {
	runContainer, err := s.getAgentContainer(agent)
	if err != nil {
		klog.Errorf("获取 agent 容器失败 %v", err)
		return err
	}

	var (
		needUpdated bool
	)
	switch agent.Status {
	case model.RestartingAgentType:
		klog.Infof("agent(%s)重启中", agent.Name)
		if err = s.restartAgentContainer(agent); err != nil {
			return err
		}
		needUpdated = true
	case model.UpgradeAgentType:
		klog.Infof("agent(%s)升级中", agent.Name)
		if runContainer != nil {
			if err = s.removeAgentContainer(agent); err != nil {
				return err
			}
		}

		if err = s.resetGithubAction(agent); err != nil {
			return err
		}
		if err = s.runAgentContainer(agent); err != nil {
			return err
		}
		needUpdated = true
	case model.RunAgentType:
		// 当数据库状态为运行，但是底层 agent 未启动的时候，直接启动
		klog.V(1).Infof("agent(%s)运行中", agent.Name)
		if runContainer == nil {
			if err = s.prepareConfig(agent); err != nil {
				return err
			}
			if err = s.runAgentContainer(agent); err != nil {
				return err
			}
		} else {
			// 检查期望状态和实际状态是否一致
			// 目前仅检查镜像是否有变动
			image := runContainer.Image
			desireImage := s.cfg.Rainbowd.AgentImage
			if image != desireImage {
				klog.Infof("已运行agent(%s)的镜像发生改动(%s)——>(%s),容器即将重建", agent.Name, image, desireImage)
				if err = s.removeAgentContainer(agent); err != nil {
					klog.Errorf("删除容器(%s)失败 %v", agent.Name, err)
					return err
				}
				if err = s.runAgentContainer(agent); err != nil {
					klog.Errorf("运行容器(%s)失败 %v", agent.Name, err)
					return err
				}
			}
		}
	case model.DeletingAgentType:
		// agent 状态是删除，容器存在则删除容器
		klog.Infof("agent(%s)删除中", agent.Name)
		if runContainer != nil {
			if err = s.removeAgentContainer(agent); err != nil {
				klog.Warningf("删除agent(%s)失败，继续删除", agent.Name) // 及时删除失败也不终止主流程
			}
		}
		if err = s.factory.Agent().Delete(context.TODO(), agent.Id); err != nil {
			return err
		}
	case model.StartingAgentType:
		klog.Infof("agent(%s)启动中", agent.Name)
		if runContainer == nil {
			if err = s.prepareConfig(agent); err != nil {
				klog.Errorf("prepare agent Config 失败 %v", err)
				return err
			}
			if err = s.runAgentContainer(agent); err != nil {
				klog.Errorf("start agent container 失败 %v", err)
				return err
			}
		} else {
			if err = s.startAgentContainer(agent); err != nil {
				klog.Errorf("start agent container 失败 %v", err)
				return err
			}
		}
		needUpdated = true
	case model.UnStartType, model.UnRunAgentType, model.StoppingAgentType:
		klog.Infof("agent(%s)停止中", agent.Name)
		if runContainer != nil {
			klog.Infof("已存在的agent将被清理")
			if err = s.stopAgentContainer(agent); err != nil {
				klog.Errorf("停止 agent 容器 %s 失败 %v", agent.Name, err)
			}
		}
		if agent.Status == model.StoppingAgentType {
			if err = s.factory.Agent().Update(context.TODO(), agent.Id, agent.ResourceVersion, map[string]interface{}{"status": model.UnRunAgentType}); err != nil {
				return err
			}
		}
	case model.OfflineAgentType:
		klog.Infof("agent(%s)下线中", agent.Name)
		if runContainer != nil {
			if err = s.removeAgentContainer(agent); err != nil {
				klog.Warningf("删除agent(%s)失败，继续删除", agent.Name) // 及时删除失败也不终止主流程
			}
		}
		// 清理本地文件
		destDir := filepath.Join(s.cfg.Rainbowd.DataDir, agent.Name)
		klog.V(1).Infof("agent 工作目录(%s) 正在被回收", destDir)
		util.RemoveFile(destDir)
		if err = s.factory.Agent().Update(context.TODO(), agent.Id, agent.ResourceVersion, map[string]interface{}{"status": model.UnRunAgentType}); err != nil {
			return err
		}
	default:
		klog.V(1).Infof("未命中 agent(%s) 状态(%s) 等待下次协同", agent.Name, agent.Status)
	}

	if needUpdated {
		if err = s.factory.Agent().Update(context.TODO(), agent.Id, agent.ResourceVersion, map[string]interface{}{"status": model.RunAgentType}); err != nil {
			return err
		}
	}
	return nil
}

func (s *rainbowdController) getAgentContainer(agent *model.Agent) (*types.Container, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	cs, err := cli.ContainerList(context.TODO(), types.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}

	for _, c := range cs {
		for _, name := range c.Names {
			if name == "/"+agent.Name {
				return &c, nil
			}
		}
	}
	return nil, nil
}

// 1. 删除 github 的 .github 文件夹
// 2. 执行 git init 重新初始化 git 配置
// 3. 输入账号信息
// 4. 重新推送至 github 完成初始化
func (s *rainbowdController) resetGithubAction(agent *model.Agent) error {
	if err := s.prepareConfig(agent); err != nil {
		return err
	}

	// 工作文件夹在 prepareConfig 中已经初始化
	destDir := filepath.Join(s.cfg.Rainbowd.DataDir, agent.Name)
	pluginDir := filepath.Join(destDir, "plugin")

	// 1. 删除 github 的 .github 文件夹
	util.RemoveFile(pluginDir + "/.git")

	// 2. 启动升级 agent 容器，初始化 git
	if err := s.runUpgradeAgent(agent); err != nil {
		return err
	}

	cmd := []string{"docker", "rm", "upgrade-" + agent.Name, "-f"}
	if err := s.runCmd(cmd); err != nil {
		return err
	}
	return nil
}

func (s *rainbowdController) prepareConfig(agent *model.Agent) error {
	agentName := agent.Name
	// 准备工作文件夹
	destDir := filepath.Join(s.cfg.Rainbowd.DataDir, agentName)
	if err := util.EnsureDirectoryExists(destDir); err != nil {
		return err
	}

	// 拷贝 plugin
	if !util.IsDirectoryExists(destDir + "/plugin") {
		if err := util.Copy(s.cfg.Rainbowd.TemplateDir+"/plugin", destDir); err != nil {
			return err
		}
	}
	// 拷贝 agent，每次都重置最新
	if err := util.Copy(s.cfg.Rainbowd.TemplateDir+"/agent", destDir); err != nil {
		return err
	}
	// 配置文件 config.yaml
	data, err := util.ReadFromFile(s.cfg.Rainbowd.TemplateDir + "/config.yaml")
	if err != nil {
		return err
	}
	var cfg rainbowconfig.Config
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}

	// 追加差异化配置
	cfg.Agent.Name = agentName
	cfgData, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err = util.WriteIntoFile(string(cfgData), destDir+"/config.yaml"); err != nil {
		return err
	}

	// 渲染 .git/config
	gc := struct{ URL string }{URL: fmt.Sprintf("https://%s:%s@github.com/%s/plugin.git", agent.GithubUser, agent.GithubToken, agent.GithubUser)}
	tpl := template.New(agentName)
	t := template.Must(tpl.Parse(GitConfig))

	var buf bytes.Buffer
	if err = t.Execute(&buf, gc); err != nil {
		return err
	}
	if err = ioutil.WriteFile(destDir+"/plugin/.git/config", buf.Bytes(), 0644); err != nil {
		return err
	}

	return nil
}

const GitConfig = `[core]
	repositoryformatversion = 0
	filemode = true
	bare = false
	logallrefupdates = true
[remote "origin"]
	url = {{ .URL }}
	fetch = +refs/heads/*:refs/remotes/origin/*
`
