package k3sbasedupgrade

import (
	"fmt"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	v33 "github.com/rancher/rancher/pkg/apis/project.cattle.io/v3"
	helmcfg "github.com/rancher/rancher/pkg/catalogv2/helm"
	v3 "github.com/rancher/rancher/pkg/generated/norman/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/wrangler"
	planv1 "github.com/rancher/system-upgrade-controller/pkg/apis/upgrade.cattle.io/v1"
	"helm.sh/helm/v3/pkg/action"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
)

const (
	// PSPAnswersField is passed to the helm --set command and denotes if we want to enable PodSecurityPolicies
	// when deploying the app, overriding the default value of 'true'.
	// In clusters >= 1.25 PSP's are not available, however we should
	// continue to deploy them in sub 1.25 clusters as they are required for cluster hardening.
	PSPAnswersField = "global.cattle.psp.enabled"
)

func (h *handler) onClusterChange(key string, cluster *v3.Cluster) (*v3.Cluster, error) {
	if cluster == nil || cluster.DeletionTimestamp != nil {
		return nil, nil
	}
	isK3s := cluster.Status.Driver == v32.ClusterDriverK3s
	isRke2 := cluster.Status.Driver == v32.ClusterDriverRke2
	// only applies to k3s/rke2 clusters
	if !isK3s && !isRke2 {
		return cluster, nil
	}
	// Don't allow nil configs to continue for given cluster type
	if (isK3s && cluster.Spec.K3sConfig == nil) || (isRke2 && cluster.Spec.Rke2Config == nil) {
		return cluster, nil
	}

	var (
		updateVersion string
		strategy      v32.ClusterUpgradeStrategy
	)
	switch {
	case isK3s:
		updateVersion = cluster.Spec.K3sConfig.Version
		strategy = cluster.Spec.K3sConfig.ClusterUpgradeStrategy
	case isRke2:
		updateVersion = cluster.Spec.Rke2Config.Version
		strategy = cluster.Spec.Rke2Config.ClusterUpgradeStrategy

	}
	if updateVersion == "" {
		return cluster, nil
	}

	// Check if the cluster is undergoing a Kubernetes version upgrade, and that
	// all downstream nodes also need the upgrade
	isNewer, err := IsNewerVersion(cluster.Status.Version.GitVersion, updateVersion)
	if err != nil {
		return cluster, err
	}
	if !isNewer {
		needsUpgrade, err := h.nodesNeedUpgrade(cluster, updateVersion)
		if err != nil {
			return cluster, err
		}
		if !needsUpgrade {
			// if upgrade was in progress, make sure to set the state back
			if v32.ClusterConditionUpgraded.IsUnknown(cluster) {
				v32.ClusterConditionUpgraded.True(cluster)
				v32.ClusterConditionUpgraded.Message(cluster, "")
				return h.clusterClient.Update(cluster)
			}
			return cluster, nil
		}

	}

	// set cluster upgrading status
	cluster, err = h.modifyClusterCondition(cluster, planv1.Plan{}, planv1.Plan{}, strategy)
	if err != nil {
		return cluster, err
	}

	// create or update k3supgradecontroller if necessary
	if err = h.deployK3sBasedUpgradeController(cluster.Name, updateVersion, isK3s, isRke2); err != nil {
		return cluster, err
	}

	// deploy plans into downstream cluster
	if err = h.deployPlans(cluster, isK3s, isRke2); err != nil {
		return cluster, err
	}

	return cluster, nil
}

// deployK3sBaseUpgradeController creates a rancher k3s/rke2 upgrader controller if one does not exist.
// Updates k3s upgrader controller if one exists and is not the newest available version.
func (h *handler) deployK3sBasedUpgradeController(clusterName, updateVersion string, isK3s, isRke2 bool) error {
	userCtx, err := h.manager.UserContextNoControllers(clusterName)
	if err != nil {
		return err
	}

	// TODO - FIND A WAY OF GETTING THIS FROM THE DOWNSTREAM  CLUSTER.
	s := userCtx.Management.Management.Settings("")
	upgradeTest, err := s.Get("system-upgrade-test", metav1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}
	if upgradeTest.Value != updateVersion {
		settingsCopy := upgradeTest.DeepCopy()
		settingsCopy.Value = updateVersion
		if _, err := s.Update(settingsCopy); err != nil {
			panic(err)
		}
	}

	// Interact with helm
	cache := memory.NewMemCacheClient(userCtx.K8sClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(cache)

	restClientGetter := &wrangler.SimpleRESTClientGetter{
		ClientConfig:    nil, // The Manager don't use the ClientConfig. Therefore, we can pass this as nill.
		RESTConfig:      &userCtx.RESTConfig,
		CachedDiscovery: cache,
		RESTMapper:      restMapper,
	}

	helmClient := helmcfg.NewClient(restClientGetter)

	// Other Aactions.
	// 	action.ListPendingInstall|action.ListPendingUpgrade|action.ListPendingRollback

	shouldPspBeEnabled, err := Is125OrAbove(updateVersion)

	for { // This can be improved, first draft.
		time.Sleep(5 * time.Second)
		releasedCharts, err := helmClient.ListReleases("cattle-system", "system-upgrade-controller", action.ListDeployed)
		if err != nil {
			return err
		}
		for _, release := range releasedCharts {
			// Ignore other charts.
			if release.Name != "system-upgrade-controller" {
				continue
			}
			// Get the chart config.
			values := release.Config
			g, e := values["global"].(map[string]interface{})
			if !e {
				continue
			}
			psp, e := g["psp"].(map[string]interface{})
			if !e {
				continue
			}
			isEnabled, e := psp["enabled"].(bool)
			if !e {
				continue
			}

			if err != nil {
				return err
			}
			// Check if the downstream is equal what it is suposed to be.
			if isEnabled == shouldPspBeEnabled {
				return nil
			} else {
				break // If it is not right we can break the inner loop, going back to sleep.
			}
		}
	}
	return nil
}

// Is125OrAbove determines if a particular Kubernetes version is
// equal to or greater than 1.25.0
func Is125OrAbove(version string) (bool, error) {
	return IsNewerVersion("v1.24.99", version)
}

// IsNewerVersion returns true if updated versions semver is newer and false if its
// semver is older. If semver is equal then metadata is alphanumerically compared.
func IsNewerVersion(prevVersion, updatedVersion string) (bool, error) {
	parseErrMsg := "failed to parse version: %v"
	prevVer, err := semver.NewVersion(strings.TrimPrefix(prevVersion, "v"))
	if err != nil {
		return false, fmt.Errorf(parseErrMsg, err)
	}

	updatedVer, err := semver.NewVersion(strings.TrimPrefix(updatedVersion, "v"))
	if err != nil {
		return false, fmt.Errorf(parseErrMsg, err)
	}

	switch updatedVer.Compare(*prevVer) {
	case -1:
		return false, nil
	case 1:
		return true, nil
	default:
		// using metadata to determine precedence is against semver standards
		// this is ignored because it because k3s uses it to precedence between
		// two versions based on same k8s version
		return updatedVer.Metadata > prevVer.Metadata, nil
	}
}

// nodeNeedsUpgrade checks all nodes in cluster, returns true if they still need to be upgraded
func (h *handler) nodesNeedUpgrade(cluster *v3.Cluster, version string) (bool, error) {
	v3NodeList, err := h.nodeLister.List(cluster.Name, labels.Everything())
	if err != nil {
		return false, err
	}
	for _, node := range v3NodeList {
		isNewer, err := IsNewerVersion(node.Status.InternalNodeStatus.NodeInfo.KubeletVersion, version)
		if err != nil {
			return false, err
		}
		if isNewer {
			return true, nil
		}
	}
	return false, nil
}

func checkDeployed(app *v33.App) bool {
	return v33.AppConditionDeployed.IsTrue(app) || v33.AppConditionInstalled.IsTrue(app)
}
