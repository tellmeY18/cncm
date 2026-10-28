package main

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Each BuildKit Job may use up to 2 CPUs and 5 GiB (see manifests/build.yaml).
var buildCPULimit = resource.MustParse("2")
var buildMemoryLimit = resource.MustParse("5Gi")

// buildMemoryHeadroom is kept free for the kernel, page cache and registry pushes.
var buildMemoryHeadroom = resource.MustParse("1Gi")

type BuildPlan struct {
	Parallel bool
	Reason   string
}

// decideBuildPlan allows both builds at once only when the node can give each its
// full limit on top of what other pods already request. Requests, not live usage,
// are used so the decision is stable and never over-commits the node.
func decideBuildPlan(allocCPU, allocMem, usedCPU, usedMem resource.Quantity) BuildPlan {
	freeCPU, freeMem := allocCPU.DeepCopy(), allocMem.DeepCopy()
	freeCPU.Sub(usedCPU)
	freeMem.Sub(usedMem)
	needCPU, needMem := buildCPULimit.DeepCopy(), buildMemoryLimit.DeepCopy()
	needCPU.Add(buildCPULimit)
	needMem.Add(buildMemoryLimit)
	needMem.Add(buildMemoryHeadroom)
	detail := fmt.Sprintf("node has %s CPU and %s free to request; two builds need %s CPU and %s", freeCPU.String(), gib(freeMem), needCPU.String(), gib(needMem))
	if freeCPU.Cmp(needCPU) >= 0 && freeMem.Cmp(needMem) >= 0 {
		return BuildPlan{Parallel: true, Reason: detail}
	}
	return BuildPlan{Reason: "one at a time: " + detail}
}

func gib(q resource.Quantity) string { return fmt.Sprintf("%.1f GiB", float64(q.Value())/(1<<30)) }

// buildPlan reads the single node's allocatable capacity and what every pod
// already requests. If anything cannot be read it chooses the safe sequential path.
func (k *Cluster) buildPlan(ctx context.Context) BuildPlan {
	nodes, err := k.client.Resource(gvr("", "v1", "nodes")).List(ctx, metav1.ListOptions{})
	if err != nil || len(nodes.Items) != 1 {
		return BuildPlan{Reason: "one at a time: node capacity unavailable"}
	}
	cpuStr, _, _ := unstructured.NestedString(nodes.Items[0].Object, "status", "allocatable", "cpu")
	memStr, _, _ := unstructured.NestedString(nodes.Items[0].Object, "status", "allocatable", "memory")
	allocCPU, e1 := resource.ParseQuantity(cpuStr)
	allocMem, e2 := resource.ParseQuantity(memStr)
	if e1 != nil || e2 != nil {
		return BuildPlan{Reason: "one at a time: node capacity unreadable"}
	}
	list, err := k.client.Resource(pods).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return BuildPlan{Reason: "one at a time: cannot list pods"}
	}
	var usedCPU, usedMem resource.Quantity
	for _, p := range list.Items {
		phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
		for _, raw := range containers {
			c, _ := raw.(map[string]any)
			req, _, _ := unstructured.NestedStringMap(c, "resources", "requests")
			if q, err := resource.ParseQuantity(req["cpu"]); err == nil {
				usedCPU.Add(q)
			}
			if q, err := resource.ParseQuantity(req["memory"]); err == nil {
				usedMem.Add(q)
			}
		}
	}
	return decideBuildPlan(allocCPU, allocMem, usedCPU, usedMem)
}
