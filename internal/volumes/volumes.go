// Package volumes analyzes PV/PVC/StorageClass safety across an
// upcoming Kubernetes upgrade — the data-loss surface that breaks
// stateful workloads and that no managed provider's pre-flight catches.
//
// Detection focus:
//
//   - In-tree → CSI driver migration (1.27 GA, in-tree removed in 1.31+).
//     If a StorageClass uses an in-tree provisioner AND the matching
//     CSI driver is not installed, post-upgrade volume operations break.
//
//   - StatefulSet single-replica risk: a StatefulSet with replicas=1
//     and PVCs gets a hard outage during node-drain rollout. The data
//     survives (PVC reattaches), but the workload is unavailable until
//     reschedule completes.
//
//   - Deployment-with-PVC pattern: a Deployment using a PVC will
//     re-bind a fresh Pod to the same PVC, but for ReadWriteOnce
//     volumes the old Pod must terminate before the new one starts —
//     and on cloud providers the volume detach/attach can take 60-300s
//     per node drain. We warn so users can plan a maintenance window.
//
//   - ReadWriteMany on cloud: AWS EFS / Azure Files in-tree drivers
//     are mid-removal; RWX PVCs on those classes need CSI before bumping.
//
//   - Pending PVCs: any PVC stuck in Pending will block upgrade-time
//     pod rescheduling. Surface them so they get fixed first.
//
//   - VolumeSnapshotClass / CSIDriver presence — required for
//     pre-upgrade snapshot via Velero or vCluster snapshot.
package volumes

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/saiyam1814/upgrade/internal/finding"
	"github.com/saiyam1814/upgrade/internal/rules/apis"
)

// Provisioner describes one in-tree → CSI migration entry.
type Provisioner struct {
	InTree        string      // e.g. "kubernetes.io/aws-ebs"
	CSI           string      // e.g. "ebs.csi.aws.com"
	MigratedIn    apis.Semver // K8s minor where migration went GA
	InTreeRemoved apis.Semver // K8s minor where in-tree is finally removed (zero if still present)
	HumanName     string      // "AWS EBS"
}

// migrations is the canonical in-tree → CSI map.
//
// Sources: https://kubernetes.io/blog/2022/09/02/cosi-kubernetes-object-storage-management/
// and per-driver KEPs.
var migrations = []Provisioner{
	{InTree: "kubernetes.io/aws-ebs", CSI: "ebs.csi.aws.com", MigratedIn: apis.MustParse("v1.27"), InTreeRemoved: apis.MustParse("v1.31"), HumanName: "AWS EBS"},
	{InTree: "kubernetes.io/gce-pd", CSI: "pd.csi.storage.gke.io", MigratedIn: apis.MustParse("v1.27"), InTreeRemoved: apis.MustParse("v1.31"), HumanName: "GCE PD"},
	{InTree: "kubernetes.io/azure-disk", CSI: "disk.csi.azure.com", MigratedIn: apis.MustParse("v1.27"), InTreeRemoved: apis.MustParse("v1.31"), HumanName: "Azure Disk"},
	{InTree: "kubernetes.io/azure-file", CSI: "file.csi.azure.com", MigratedIn: apis.MustParse("v1.27"), InTreeRemoved: apis.MustParse("v1.32"), HumanName: "Azure File"},
	{InTree: "kubernetes.io/vsphere-volume", CSI: "csi.vsphere.vmware.com", MigratedIn: apis.MustParse("v1.27"), InTreeRemoved: apis.MustParse("v1.32"), HumanName: "vSphere"},
	{InTree: "kubernetes.io/cinder", CSI: "cinder.csi.openstack.org", MigratedIn: apis.MustParse("v1.26"), InTreeRemoved: apis.MustParse("v1.31"), HumanName: "OpenStack Cinder"},
	{InTree: "kubernetes.io/portworx-volume", CSI: "pxd.portworx.com", MigratedIn: apis.MustParse("v1.31"), InTreeRemoved: apis.MustParse("v1.34"), HumanName: "Portworx"},
}

// Analyze runs every volume safety check and returns findings.
func Analyze(ctx context.Context, core kubernetes.Interface, target apis.Semver) ([]finding.Finding, []error) {
	var (
		out  []finding.Finding
		errs []error
	)

	scs, err := core.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, []error{fmt.Errorf("list storageclasses: %w", err)}
	}
	pvcs, err := core.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, []error{fmt.Errorf("list pvcs: %w", err)}
	}
	deps, _ := core.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	stsList, _ := core.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	csiDrivers := installedCSIDrivers(ctx, core)
	scByName := indexStorageClasses(scs.Items)

	// 1. StorageClass: in-tree provisioner without matching CSI driver.
	for _, sc := range scs.Items {
		mig := matchMigration(sc.Provisioner)
		if mig == nil {
			continue
		}
		// If target is at or past in-tree removal AND CSI driver missing → BLOCKER.
		if !mig.InTreeRemoved.Equal(apis.Semver{}) && !target.Less(mig.InTreeRemoved) && !csiDrivers[mig.CSI] {
			out = append(out, finding.Finding{
				Severity: finding.Blocker,
				Category: finding.CategoryDefault,
				Title:    fmt.Sprintf("StorageClass %q uses %s in-tree provisioner; CSI driver %q not installed (target %s removes in-tree)", sc.Name, mig.HumanName, mig.CSI, target),
				Detail:   fmt.Sprintf("In-tree provisioner %s is removed in K8s %s. PVCs using this StorageClass will fail to provision/attach after upgrade.", mig.InTree, mig.InTreeRemoved),
				Source:   finding.Source{Kind: "live", Location: "storage.k8s.io/v1/storageclasses/" + sc.Name},
				Object:   &finding.Object{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: sc.Name},
				Fix:      fmt.Sprintf("Install the %s CSI driver BEFORE upgrading. See provider docs.", mig.HumanName),
				Docs:     []string{"https://kubernetes.io/docs/concepts/storage/volumes/#csi-migration"},
			})
		} else if !target.Less(mig.MigratedIn) && !csiDrivers[mig.CSI] {
			// Migration is GA but in-tree still present; CSI driver missing → HIGH.
			out = append(out, finding.Finding{
				Severity: finding.High,
				Category: finding.CategoryDefault,
				Title:    fmt.Sprintf("StorageClass %q: %s migrated to CSI; driver %q not installed", sc.Name, mig.HumanName, mig.CSI),
				Detail:   "Migration is silent today but in-tree code path will be removed soon. Volumes work for now; install the CSI driver before the next upgrade.",
				Source:   finding.Source{Kind: "live", Location: "storage.k8s.io/v1/storageclasses/" + sc.Name},
				Object:   &finding.Object{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass", Name: sc.Name},
				Fix:      fmt.Sprintf("Install %s CSI driver to enable native CSI provisioning.", mig.HumanName),
				Docs:     []string{"https://kubernetes.io/docs/concepts/storage/volumes/#csi-migration"},
			})
		}
	}

	// 2. PVCs in Pending — will block reschedule on drain.
	for _, pvc := range pvcs.Items {
		if pvc.Status.Phase == corev1.ClaimPending {
			out = append(out, finding.Finding{
				Severity: finding.High,
				Category: finding.CategoryDefault,
				Title:    fmt.Sprintf("PVC %s/%s is Pending — will block pod reschedule during node drain", pvc.Namespace, pvc.Name),
				Detail:   "Pending PVCs cannot bind, so any Pod consuming this PVC will Pending after a node drain.",
				Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("v1/persistentvolumeclaims/%s/%s", pvc.Namespace, pvc.Name)},
				Object:   &finding.Object{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name},
				Fix:      "Investigate why the PVC is Pending (missing StorageClass, missing CSI driver, capacity issue) and resolve before upgrade.",
			})
		}
	}

	// 3. Deployment-with-PVC: data loss / outage risk on rollout.
	for _, d := range deps.Items {
		if !podSpecHasPVC(d.Spec.Template.Spec) {
			continue
		}
		out = append(out, finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryDefault,
			Title:    fmt.Sprintf("Deployment %s/%s mounts a PVC — wrong primitive for stateful workload", d.Namespace, d.Name),
			Detail:   "Deployments do not provide stable storage per-replica. On node drain the new Pod will wait for the old Pod to release the PVC (RWO) — visible outage. Convert to StatefulSet for stable identity + storage.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("apps/v1/deployments/%s/%s", d.Namespace, d.Name)},
			Object:   &finding.Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: d.Namespace, Name: d.Name},
			Fix:      "Convert to StatefulSet, OR scale to 0 before upgrade and accept data-during-rollout downtime.",
			Docs:     []string{"https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/"},
		})
	}

	// 4. StatefulSet single-replica — outage but no data loss.
	for _, s := range stsList.Items {
		if s.Spec.Replicas == nil || *s.Spec.Replicas != 1 {
			continue
		}
		if len(s.Spec.VolumeClaimTemplates) == 0 && !podSpecHasPVC(s.Spec.Template.Spec) {
			continue
		}
		out = append(out, finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryPDB,
			Title:    fmt.Sprintf("StatefulSet %s/%s has replicas=1 with persistent storage — node drain = downtime", s.Namespace, s.Name),
			Detail:   "Single-replica stateful workloads always have an outage window during node drains (volume detach/attach). Data is safe; availability is not.",
			Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("apps/v1/statefulsets/%s/%s", s.Namespace, s.Name)},
			Object:   &finding.Object{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: s.Namespace, Name: s.Name},
			Fix:      "Plan a maintenance window OR scale to ≥ 2 replicas if the workload supports HA. Verify your backup is fresh.",
		})
	}

	// 5. RWX PVCs on classes whose driver is mid-removal.
	for _, pvc := range pvcs.Items {
		if !hasRWX(pvc.Spec.AccessModes) {
			continue
		}
		scName := pvcStorageClassName(pvc)
		if scName == "" {
			continue
		}
		sc, ok := scByName[scName]
		if !ok {
			continue
		}
		mig := matchMigration(sc.Provisioner)
		if mig == nil {
			continue
		}
		if !target.Less(mig.InTreeRemoved) && !csiDrivers[mig.CSI] {
			out = append(out, finding.Finding{
				Severity: finding.Blocker,
				Category: finding.CategoryDefault,
				Title:    fmt.Sprintf("RWX PVC %s/%s on %s without %s CSI driver — broken after upgrade", pvc.Namespace, pvc.Name, mig.HumanName, mig.HumanName),
				Detail:   "ReadWriteMany volumes are especially sensitive to in-tree → CSI migration; multi-attach failure is the typical symptom.",
				Source:   finding.Source{Kind: "live", Location: fmt.Sprintf("v1/persistentvolumeclaims/%s/%s", pvc.Namespace, pvc.Name)},
				Object:   &finding.Object{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: pvc.Namespace, Name: pvc.Name},
				Fix:      fmt.Sprintf("Install %s CSI driver and verify RWX support before upgrading.", mig.HumanName),
			})
		}
	}

	// 6. Released PVs that haven't been reclaimed — surface as INFO.
	pvs, err := core.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, pv := range pvs.Items {
			if pv.Status.Phase == corev1.VolumeReleased {
				out = append(out, finding.Finding{
					Severity: finding.Low,
					Category: finding.CategoryDefault,
					Title:    fmt.Sprintf("PV %s is Released (not reclaimed)", pv.Name),
					Detail:   "Released PVs hold underlying storage. Cleanup or reclaim before upgrade to avoid surprise costs and stale state.",
					Source:   finding.Source{Kind: "live", Location: "v1/persistentvolumes/" + pv.Name},
					Object:   &finding.Object{APIVersion: "v1", Kind: "PersistentVolume", Name: pv.Name},
					Fix:      "kubectl delete pv " + pv.Name + " (after confirming the underlying disk is no longer needed).",
				})
			}
		}
	} else if !apierrors.IsForbidden(err) {
		errs = append(errs, fmt.Errorf("list pvs: %w", err))
	}

	// 7. CSI snapshot capability — needed for safe pre-upgrade snapshots.
	if !hasVolumeSnapshotClass(ctx, core) {
		out = append(out, finding.Finding{
			Severity: finding.Medium,
			Category: finding.CategoryBackup,
			Title:    "No VolumeSnapshotClass installed — pre-upgrade snapshots not available",
			Detail:   "Without a VolumeSnapshotClass + snapshot controller, you cannot take consistent snapshots of stateful workloads before upgrading. Velero file-level backup is your fallback.",
			Source:   finding.Source{Kind: "live", Location: "snapshot.storage.k8s.io"},
			Fix:      "Install snapshot-controller (kubernetes-csi/external-snapshotter) and create a VolumeSnapshotClass for your CSI driver.",
			Docs:     []string{"https://kubernetes-csi.github.io/docs/snapshot-controller.html"},
		})
	}

	return out, errs
}

// ---- helpers ----

func matchMigration(provisioner string) *Provisioner {
	for i := range migrations {
		if migrations[i].InTree == provisioner {
			return &migrations[i]
		}
	}
	return nil
}

func indexStorageClasses(scs []storagev1.StorageClass) map[string]storagev1.StorageClass {
	out := make(map[string]storagev1.StorageClass, len(scs))
	for _, sc := range scs {
		out[sc.Name] = sc
	}
	return out
}

func installedCSIDrivers(ctx context.Context, core kubernetes.Interface) map[string]bool {
	out := map[string]bool{}
	cd, err := core.StorageV1().CSIDrivers().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, d := range cd.Items {
		out[d.Name] = true
	}
	return out
}

func podSpecHasPVC(spec corev1.PodSpec) bool {
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return true
		}
	}
	return false
}

func hasRWX(modes []corev1.PersistentVolumeAccessMode) bool {
	for _, m := range modes {
		if m == corev1.ReadWriteMany {
			return true
		}
	}
	return false
}

func pvcStorageClassName(pvc corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName != nil && *pvc.Spec.StorageClassName != "" {
		return *pvc.Spec.StorageClassName
	}
	return pvc.Annotations["volume.beta.kubernetes.io/storage-class"]
}

func hasVolumeSnapshotClass(ctx context.Context, core kubernetes.Interface) bool {
	// We don't import the snapshot client to avoid a heavy dep — probe
	// CRD presence via the discovery client through a list-call hack.
	// Best-effort: presence of the CRD itself is a strong signal.
	_, err := core.Discovery().ServerResourcesForGroupVersion("snapshot.storage.k8s.io/v1")
	return err == nil
}

// migrationName returns the human label for a provisioner string.
func migrationName(provisioner string) string {
	if strings.HasPrefix(provisioner, "kubernetes.io/") {
		if m := matchMigration(provisioner); m != nil {
			return m.HumanName
		}
	}
	return provisioner
}

var _ = migrationName // exposed-but-unused helper retained for renderer
