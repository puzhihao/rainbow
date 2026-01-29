package builder

import (
	"context"
	"encoding/base64"
	"fmt"
	"github.com/caoyingjunz/pixiulib/exec"
	"github.com/caoyingjunz/rainbow/cmd/app/config"
	"github.com/caoyingjunz/rainbow/pkg/util"
	"github.com/docker/docker/client"
	"k8s.io/klog/v2"
	"os"
	"time"
)

type BuilderController struct {
	Callback   string
	RegistryId int64
	BuilderId  int64
	DockerFile string
	Repo       string

	httpClient util.HttpInterface
	exec       exec.Interface
	docker     *client.Client

	Cfg      config.Config
	Registry config.Registry
	Images   config.Image
}

func (b *BuilderController) Login() error {
	cmd := []string{"docker", "login", "-u", b.Registry.Username, "-p", b.Registry.Password}
	if b.Registry.Repository != "" {
		cmd = append(cmd, b.Registry.Repository)
	}
	out, err := b.exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to login in image %v %v", string(out), err)
	}
	klog.Infof("镜像仓库登录完成")
	return nil
}

func NewBuilderController(cfg config.Config) *BuilderController {
	return &BuilderController{
		Cfg:        cfg,
		Callback:   cfg.Builder.Callback,
		BuilderId:  cfg.Builder.BuilderId,
		RegistryId: cfg.Builder.RegistryId,
		Repo:       cfg.Builder.Repo,
		DockerFile: cfg.Builder.Dockerfile,
		httpClient: util.NewHttpClient(5*time.Second, cfg.Plugin.Callback),
	}
}

func (b *BuilderController) Complete() error {
	var err error
	if err = b.doComplete(); err != nil {
		klog.Info(err)
	}
	return nil
}

func (b *BuilderController) Close() {
	if b.docker != nil {
		_ = b.docker.Close()
	}
}

func (b *BuilderController) doComplete() error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	b.docker = cli
	b.exec = exec.New()
	b.Registry = b.Cfg.Registry

	return b.Validate()
}

func (b *BuilderController) BuildAndPushImage() error {
	imageName := fmt.Sprintf(
		"%s/%s/%s",
		b.Registry.Repository,
		b.Registry.Namespace,
		b.Repo,
	)

	dockerfileBytes, err := base64.StdEncoding.DecodeString(b.DockerFile)
	if err != nil {
		return fmt.Errorf("decode dockerfile failed: %w", err)
	}

	err = os.WriteFile("Dockerfile", dockerfileBytes, 0644)
	if err != nil {
		return fmt.Errorf("write Dockerfile failed: %w", err)
	}

	klog.Infof("Starting build image %s", imageName)
	buildCmd := []string{"docker", "build", "-t", imageName, "."}

	out, err := b.exec.Command(buildCmd[0], buildCmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to build image: %w\n%s", err, string(out))
	}

	klog.Infof("Image built successfully: %s", imageName)

	klog.Infof("Starting push image %s", imageName)
	pushCmd := []string{"docker", "push", imageName}

	out, err = b.exec.Command(pushCmd[0], pushCmd[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to push image: %w\n%s", err, string(out))
	}

	klog.Infof("Image pushed successfully: %s", imageName)
	return nil
}

func (b *BuilderController) Validate() error {
	if _, err := b.docker.Ping(context.Background()); err != nil {
		klog.Errorf("%v", err)
		return err
	}
	klog.Infof("plugin validate completed")
	return nil
}

func (b *BuilderController) Run() error {
	err := b.Login()
	if err != nil {
		return err
	}
	err = b.BuildAndPushImage()
	if err != nil {
		return err
	}

	return nil
}
