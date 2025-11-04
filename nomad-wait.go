package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/nomad/api"
)

const (
	WaitModeAny = "any"
	WaitModeAll = "all"
)

type Alloc struct {
	*api.Allocation
	Index int
}

type AllocCache map[string]*Alloc

// 环境变量
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return i
		}
	}
	return def
}

// 解析 [index]
func parseIndex(name string) int {
	re := regexp.MustCompile(`\[(\d+)\]`)
	if m := re.FindStringSubmatch(name); len(m) > 1 {
		i, _ := strconv.Atoi(m[1])
		return i
	}
	return -1
}

// 是否 sidecar
func isSidecar(task *api.Task) bool {
	if task == nil || task.Config == nil {
		return false
	}
	_, ok := task.Config["sidecar"]
	return ok
}

// 是否健康（服务）
func isHealthy(alloc *api.Allocation) bool {
	return alloc.ClientStatus == "running" &&
		alloc.DeploymentStatus != nil &&
		alloc.DeploymentStatus.Healthy != nil &&
		*alloc.DeploymentStatus.Healthy
}

// 是否完成（批处理）
func isComplete(alloc *api.Allocation) bool {
	return alloc.ClientStatus == "complete"
}

// 获取任务组 Count
func getGroupCount(client *api.Client, jobName, group string) (int, error) {
	job, _, err := client.Jobs().Info(jobName, &api.QueryOptions{})
	if err != nil {
		return 0, err
	}
	for _, tg := range job.TaskGroups {
		if *tg.Name == group {
			return *tg.Count, nil
		}
	}
	return 0, fmt.Errorf("group not found")
}

// 加载所有活跃分配（完整信息）
func loadAllocs(client *api.Client, jobName, group string) (AllocCache, error) {
	stubs, _, err := client.Jobs().Allocations(jobName, true, nil)
	if err != nil {
		return nil, err
	}

	cache := make(AllocCache)
	for _, stub := range stubs {
		if group != "" && stub.TaskGroup != group {
			continue
		}
		if stub.ClientStatus == "complete" && stub.JobVersion < stub.DesiredVersion {
			continue // 旧版本
		}

		full, _, err := client.Allocations().Info(stub.ID, nil)
		if err != nil {
			continue
		}

		cache[full.ID] = &Alloc{full, parseIndex(full.Name)}
	}
	return cache, nil
}

// 检查状态：sidecar 用 Healthy，普通任务用 Complete 或 Healthy
func checkStatus(
	cache AllocCache,
	group string,
	jobType string,
	expectedCount int,
	mode string,
) (success, failed bool, healthy, total int, indicator string) {

	healthy = 0
	total = len(cache)
	ind := strings.Builder{}
	failed = false

	for _, a := range cache {
		if group != "" && a.TaskGroup != group {
			continue
		}

		if a.ClientStatus == "failed" {
			ind.WriteString("!")
			failed = true
			continue
		}

		// 遍历任务，判断是否 sidecar
		isSidecarTask := false
		for _, task := range a.TaskGroupTasks() {
			if isSidecar(task) {
				isSidecarTask = true
				break
			}
		}

		ok := false
		if isSidecarTask {
			// sidecar 必须 Healthy
			if isHealthy(a.Allocation) {
				ok = true
			}
		} else {
			// 普通任务：batch 用 complete，service 用 healthy
			if jobType == "batch" {
				if isComplete(a.Allocation) {
					ok = true
				}
			} else {
				if isHealthy(a.Allocation) {
					ok = true
				}
			}
		}

		if ok {
			ind.WriteString("+")
			healthy++
		} else {
			ind.WriteString("-")
		}
	}

	indicator = ind.String()

	switch mode {
	case WaitModeAny:
		success = healthy > 0
	case WaitModeAll:
		success = healthy == expectedCount && expectedCount > 0
	}

	return
}

func main() {
	// 参数
	addr := flag.String("address", env("NOMAD_ADDR", "http://127.0.0.1:4646"), "Nomad address")
	timeout := flag.Int("t", envInt("NOMAD_JOB_TIMEOUT", 0), "timeout (s)")
	group := flag.String("group", env("NOMAD_TASK_GROUP", ""), "task group")
	token := flag.String("token", env("NOMAD_TOKEN", ""), "ACL token")
	mode := flag.String("mode", env("NOMAD_WAIT_MODE", WaitModeAny), "any|all")
	flag.Parse()

	*mode = strings.ToLower(*mode)
	if *mode != WaitModeAny && *mode != WaitModeAll {
		*mode = WaitModeAny
	}

	if len(flag.Args()) != 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <job>\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}
	jobName := flag.Arg(0)

	// 客户端
	config := api.DefaultConfig()
	config.Address = *addr
	config.SecretID = *token
	client, err := api.NewClient(config)
	if err != nil {
		log.Fatalf("Client error: %v", err)
	}

	// 作业信息
	job, _, err := client.Jobs().Info(jobName, nil)
	if err != nil {
		log.Fatalf("Job not found: %v", err)
	}
	jobType := *job.Type

	// 预期数量
	expectedCount := 0
	if *group != "" {
		expectedCount, _ = getGroupCount(client, jobName, *group)
	} else {
		for _, tg := range job.TaskGroups {
			expectedCount += *tg.Count
		}
	}

	tgName := "all"
	if *group != "" {
		tgName = *group
	}
	log.Printf("WAIT job='%s' mode=%s group=%s expect=%d", jobName, *mode, tgName, expectedCount)

	// 上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; log.Println("Interrupted"); cancel() }()

	// 超时
	var timeoutCh <-chan time.Time
	if *timeout > 0 {
		timeoutCh = time.After(time.Duration(*timeout) * time.Second)
	}

	// 事件流
	es := client.EventStream()
	eventCh, _ := es.Stream(ctx, map[api.Topic][]string{api.TopicAllocation: {jobName}}, 0, nil)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			os.Exit(1)
		case <-timeoutCh:
			log.Printf("TIMEOUT after %ds", *timeout)
			os.Exit(1)

		case <-ticker.C:
			cache, err := loadAllocs(client, jobName, *group)
			if err != nil {
				log.Printf("Load error: %v", err)
				continue
			}

			success, failed, healthy, total, ind := checkStatus(cache, *group, jobType, expectedCount, *mode)

			log.Printf("[%s] group=%s expect=%d healthy=%d/%d", ind, tgName, expectedCount, healthy, total)

			if failed {
				log.Println("FAILED: detected failed allocs")
				os.Exit(1)
			}
			if success {
				log.Printf("SUCCESS: %d/%d healthy", healthy, expectedCount)
				os.Exit(0)
			}

		case events, ok := <-eventCh:
			if !ok {
				eventCh = nil
				log.Println("Event stream lost, polling")
			}
			if events != nil && len(events.Events) > 0 {
				// 任意事件触发刷新
				continue
			}
		}
	}
}
