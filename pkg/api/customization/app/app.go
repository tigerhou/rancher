package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rancher/norman/api/access"
	"github.com/rancher/norman/parse"
	"github.com/rancher/norman/types"
	"github.com/rancher/norman/types/convert"
	"github.com/rancher/rancher/pkg/auth/tokens"
	"github.com/rancher/rancher/pkg/controllers/management/compose/common"
	hutils "github.com/rancher/rancher/pkg/controllers/user/helm/utils"
	"github.com/rancher/types/apis/management.cattle.io/v3"
	managementschema "github.com/rancher/types/apis/management.cattle.io/v3/schema"
	projectschema "github.com/rancher/types/apis/project.cattle.io/v3/schema"
	"github.com/rancher/types/client/management/v3"
	managementv3 "github.com/rancher/types/client/management/v3"
	projectv3 "github.com/rancher/types/client/project/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	helmName  = "helm"
	cacheRoot = "helm-controller"
)

type ActionWrapper struct {
	Clusters         v3.ClusterInterface
	KubeConfigGetter common.KubeConfigGetter
}

func Formatter(apiContext *types.APIContext, resource *types.RawResource) {
	resource.AddAction(apiContext, "upgrade")
	resource.AddAction(apiContext, "rollback")
}

func (a ActionWrapper) ActionHandler(actionName string, action *types.Action, apiContext *types.APIContext) error {
	var app projectv3.App
	if err := access.ByID(apiContext, &projectschema.Version, projectv3.AppType, apiContext.ID, &app); err != nil {
		return err
	}
	clusterName := strings.Split(app.ProjectId, ":")[0]
	var cluster managementv3.Cluster
	if err := access.ByID(apiContext, &managementschema.Version, managementv3.ClusterType, clusterName, &cluster); err != nil {
		return err
	}
	tokenAuthValue := tokens.GetTokenAuthFromRequest(apiContext.Request)
	data := a.KubeConfigGetter.KubeConfig(cluster.ID, tokenAuthValue)
	for k := range data.Clusters {
		data.Clusters[k].InsecureSkipTLSVerify = true
	}
	rootDir, err := ioutil.TempDir("", "helm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(rootDir)
	kubeConfigPath := filepath.Join(rootDir, ".kubeconfig")
	if err := clientcmd.WriteToFile(*data, kubeConfigPath); err != nil {
		return err
	}
	store := apiContext.Schema.Store
	switch actionName {
	case "upgrade":
		cont, cancel := context.WithCancel(context.Background())
		defer cancel()
		addr := hutils.GenerateRandomPort()
		probeAddr := hutils.GenerateRandomPort()
		go hutils.StartTiller(cont, addr, probeAddr, app.InstallNamespace, kubeConfigPath)
		actionInput, err := parse.ReadBody(apiContext.Request)
		if err != nil {
			return err
		}
		externalID := actionInput["externalId"]
		updateData := map[string]interface{}{}
		updateData["externalId"] = externalID
		_, err = store.Update(apiContext, apiContext.Schema, updateData, apiContext.ID)
		if err != nil {
			return err
		}
		templateVersionID, err := hutils.ParseExternalID(convert.ToString(externalID))
		if err != nil {
			return err
		}
		var templateVersion managementv3.TemplateVersion
		if err := access.ByID(apiContext, &managementschema.Version, managementv3.TemplateVersionType, templateVersionID, &templateVersion); err != nil {
			return err
		}
		files, err := hutils.ConvertTemplates(templateVersion.Files)
		if err != nil {
			return err
		}
		tempDir, err := hutils.WriteTempDir(rootDir, files)
		if err != nil {
			return err
		}
		defer os.RemoveAll(tempDir)
		if err := upgradeCharts(tempDir, addr, app.Name); err != nil {
			return err
		}
		_, err = store.Update(apiContext, apiContext.Schema, updateData, apiContext.ID)
		if err != nil {
			return err
		}
		return nil
	case "rollback":
		cont, cancel := context.WithCancel(context.Background())
		defer cancel()
		addr := hutils.GenerateRandomPort()
		probeAddr := hutils.GenerateRandomPort()
		go hutils.StartTiller(cont, addr, probeAddr, app.InstallNamespace, kubeConfigPath)
		actionInput, err := parse.ReadBody(apiContext.Request)
		if err != nil {
			return err
		}
		revision := actionInput["revision"]
		if err := rollbackCharts(addr, app.Name, convert.ToString(revision)); err != nil {
			return err
		}
		data := map[string]interface{}{
			"name": apiContext.ID,
		}
		_, err = store.Update(apiContext, apiContext.Schema, data, apiContext.ID)
		if err != nil {
			return err
		}
		return nil
	}
	return nil
}

func upgradeCharts(rootDir, port, releaseName string) error {
	cmd := exec.Command(helmName, "upgrade", "--namespace", releaseName, releaseName, rootDir)
	cmd.Env = []string{fmt.Sprintf("%s=%s", "HELM_HOST", "127.0.0.1:"+port)}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func rollbackCharts(port, releaseName, revision string) error {
	cmd := exec.Command(helmName, "rollback", releaseName, revision)
	cmd.Env = []string{fmt.Sprintf("%s=%s", "HELM_HOST", "127.0.0.1:"+port)}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func (a ActionWrapper) toRESTConfig(cluster *client.Cluster) (*rest.Config, error) {
	cls, err := a.Clusters.Get(cluster.ID, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cls == nil {
		return nil, nil
	}

	//if cluster.Internal {
	//	return a.LocalConfig, nil
	//}

	if cluster.APIEndpoint == "" || cluster.CACert == "" || cls.Status.ServiceAccountToken == "" {
		return nil, nil
	}

	u, err := url.Parse(cluster.APIEndpoint)
	if err != nil {
		return nil, err
	}

	data, err := base64.StdEncoding.DecodeString(cluster.CACert)
	if err != nil {
		return nil, err
	}

	return &rest.Config{
		Host:        u.Host,
		Prefix:      u.Path,
		BearerToken: cls.Status.ServiceAccountToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: data,
		},
	}, nil
}
