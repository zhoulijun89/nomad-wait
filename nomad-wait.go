package main

import (
	"context"
	"errors"
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

// 状态常量
const (
	STATUS_HEALTHY   = "healthy"
	STATUS_UNHEALTHY = "unhealthy"
	STATUS_COMPLETE  = "complete"
	STATUS_FAILED    = "failed"
	STATUS_RUNNING   = "running"
)

// 等待模式
const (
	WaitModeAny = "any" // 任意一个健康就成功（默认）
	WaitModeAll = "all" // 所有都健康才成功
)

// Allocation 扩展结构
type Allocation struct {
	api.AllocationListStub
	Index int // 从 [0] 解析的索引
}

// AllocCache 缓存
type AllocCache map[string]*Allocation

// findAllocation 在列表中查找
func findAllocation(allocs []*Allocation, taskGroup string, index int) *Allocation {
	for _, a := range allocs {
		if a.TaskGroup == taskGroup && a.Index == index {
			return a
		}
	}
	return nil
}

// 环境变量工具
func stringFromEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func intFromEnv(key string, def int) (int, error) {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return def, fmt.Errorf("解析 %s 失败: %v", key, err)
		}
		return i, nil
	}
	return def, nil
}

// 解析命令行参数获取 job 名称
func jobNameFromArgs(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("必须提供且仅提供一个作业名称")
	}
	return args[0], nil
}

// 解析分配名称: job.taskgroup[0]
func parseAllocationName(name string) (jobID, taskGroup string, index int, err error) {
	re := regexp.MustCompile(`([\w-]+)\.([\w-]+)\[(\d+)\]`)
	m := re.FindStringSubmatch(name)
	if len(m) != 4 {
		return "", "", -1, fmt.Errorf("无法解析分配名称: %s", name)
	}
	index, _ = strconv.Atoi(m[3])
	return m[1], m[2], index, nil
}

// 获取任务组预期数量
func getTaskGroupCount(client *api.Client, jobName, taskGroup string) (int, error) {
	job, _, err := client.Jobs().Info(jobName, &api.QueryOptions{})
	if err != nil {
		return 0, err
	}
	for _, tg := range job.TaskGroups {
		if *tg.Name == taskGroup {
			return *tg.Count, nil
		}
	}
	return 0, fmt.Errorf("任务组 %s 未找到", taskGroup)
}

// 获取活跃分配
func monitoredAllocations(client *api.Client, jobName, group, jobType string) ([]*Allocation, error) {
	var result []*Allocation
	q := &api.QueryOptions{}

	for {
		list, meta, err := client.Allocations().List(q)
		if err != nil {
			return nil, err
		}
		for _, stub := range list {
			if stub.JobID != jobName || (group != "" && stub.TaskGroup != group) {
				continue
			}
			if jobType != "batch" && (stub.ClientStatus == STATUS_COMPLETE || stub.ClientStatus == STATUS_FAILED) {
				continue
			}
			_, _, idx, err := parseAllocationName(stub.Name)
			if err != nil {
				continue
			}
			result = append(result, &Allocation{*stub, idx})
		}
		if meta.NextToken == "" {
			break
		}
		q.NextToken = meta.NextToken
	}
	return result, nil
}

// 获取分配状态（健康/完成）
func allocationStatus(a *Allocation, jobType string) string {
	if jobType == "batch" {
		return a.ClientStatus
	}
	// 必须满足：running + 有部署状态 + Healthy 明确为 true
	if a.ClientStatus == "running" &&
		a.DeploymentStatus != nil &&
		a.DeploymentStatus.Healthy != nil &&
		*a.DeploymentStatus.Healthy {
		return STATUS_HEALTHY
	}
	// 其他情况一律视为不健康（包括 Degraded）
	return STATUS_UNHEALTHY
}

// checkStatus 支持 any/all 模式
func checkStatus(cache AllocCache, targetGroup, jobType string, expectedCount int, mode string) (success, failed bool, indicator string) {
	acceptable := STATUS_HEALTHY
	if jobType == "batch" {
		acceptable = STATUS_COMPLETE
	}

	var totalActive, healthyCount int
	ind := make([]rune, 0, len(cache))

	for _, a := range cache {
		if targetGroup != "" && a.TaskGroup != targetGroup {
			continue
		}
		if jobType != "batch" && (a.ClientStatus == STATUS_COMPLETE || a.ClientStatus == STATUS_FAILED) {
			continue
		}
		totalActive++
		status := allocationStatus(a, jobType)

		switch status {
		case STATUS_FAILED:
			failed = true
			ind = append(ind, '!')
		case acceptable:
			healthyCount++
			ind = append(ind, '+')
		default:
			ind = append(ind, '-')
		}
	}

	indicator = string(ind)

	// 数量不足
	if expectedCount > 0 && totalActive < expectedCount {
		log.Printf("[DEBUG] 活跃数 %d < 预期 %d，等待调度", totalActive, expectedCount)
		return false, failed, strings.Repeat("-", totalActive)
	}

	// 成功判断
	switch mode {
	case WaitModeAny:
		success = healthyCount > 0
	case WaitModeAll:
		success = healthyCount == totalActive && totalActive > 0
	default:
		success = healthyCount > 0 // fallback
	}

	return success, failed, indicator
}

// 事件更新缓存
func updateCacheFromEvent(cache AllocCache, ev *api.Event, jobName, group, jobType string) bool {
	if ev.Topic != api.TopicAllocation {
		return false
	}
	alloc, err := ev.Allocation()
	if err != nil || alloc == nil || alloc.JobID != jobName {
		return false
	}
	if group != "" && alloc.TaskGroup != group {
		return false
	}

	// 批处理外移除已终态
	if jobType != "batch" && (alloc.ClientStatus == STATUS_COMPLETE || alloc.ClientStatus == STATUS_FAILED) {
		delete(cache, alloc.ID)
		return true
	}

	_, _, idx, _ := parseAllocationName(alloc.Name)
	cache[alloc.ID] = &Allocation{
		AllocationListStub: api.AllocationListStub{
			ID:               alloc.ID,
			JobID:            alloc.JobID,
			TaskGroup:        alloc.TaskGroup,
			ClientStatus:     alloc.ClientStatus,
			DeploymentStatus: alloc.DeploymentStatus,
			CreateTime:       alloc.CreateTime,
		},
		Index: idx,
	}
	return true
}

// 日志设置
func setupLogger() {
	level := strings.ToLower(stringFromEnv("NOMAD_LOG_LEVEL", "info"))
	switch level {
	case "debug":
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	default:
		log.SetFlags(log.LstdFlags)
	}
	log.Printf("[INFO] 日志级别: %s", level)
}

// Run 主函数
func Run() int {
	setupLogger()

	var (
		address    string
		timeout    int
		group      string
		token      string
		waitMode   string
	)

	// 帮助文本
	flag.StringVar(&address, "address", stringFromEnv("NOMAD_ADDR", "http://127.0.0.1:4646"), "Nomad 地址")
	flag.IntVar(&timeout, "timeout", 0, "超时秒数 (0=永不超时)")
	flag.IntVar(&timeout, "t", 0, "超时秒数 (0=永不超时)")
	flag.StringVar(&group, "group", stringFromEnv("NOMAD_TASK_GROUP", ""), "任务组名")
	flag.StringVar(&token, "token", stringFromEnv("NOMAD_TOKEN", ""), "ACL Token")
	flag.StringVar(&waitMode, "mode", stringFromEnv("NOMAD_WAIT_MODE", WaitModeAny),
		"等待模式: any=任意一个健康即成功 (默认), all=全部健康才成功")

	flag.Parse()

	// 覆盖 timeout
	if t, err := intFromEnv("NOMAD_JOB_TIMEOUT", timeout); err == nil {
		timeout = t
	}

	// 验证 mode
	waitMode = strings.ToLower(strings.TrimSpace(waitMode))
	if waitMode != WaitModeAny && waitMode != WaitModeAll {
		log.Printf("[WARN] 无效 mode: %s，使用默认: %s", waitMode, WaitModeAny)
		waitMode = WaitModeAny
	}

	jobName, err := jobNameFromArgs(flag.Args())
	if err != nil {
		fmt.Printf("用法: %s [选项] <作业名>\n", os.Args[0])
		flag.PrintDefaults()
		return 1
	}

	// Nomad 客户端
	config := api.DefaultConfig()
	config.Address = address
	config.SecretID = token
	client, err := api.NewClient(config)
	if err != nil {
		log.Printf("[ERROR] 创建客户端失败: %v", err)
		return 1
	}

	// 获取作业信息
	job, _, err := client.Jobs().Info(jobName, &api.QueryOptions{})
	if err != nil {
		log.Printf("[ERROR] 获取作业失败: %v", err)
		return 1
	}
	jobType := *job.Type
	expectedCount := 0
	if group != "" {
		expectedCount, _ = getTaskGroupCount(client, jobName, group)
	}

	taskGroup := "全部"
	if group != "" {
		taskGroup = group
	}
	log.Printf("[INFO] 等待作业 '%s' (%s 模式) - 任务组: %s", jobName, waitMode, taskGroup)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; log.Println("[INFO] 收到中断"); cancel() }()

	// 初始加载
	allocs, err := monitoredAllocations(client, jobName, group, jobType)
	if err != nil || len(allocs) == 0 {
		log.Printf("[ERROR] 未找到分配: %v", err)
		return 1
	}

	cache := make(AllocCache)
	for _, a := range allocs {
		cache[a.ID] = a
	}

	success, failed, ind := checkStatus(cache, group, jobType, expectedCount, waitMode)
	if failed {
		log.Printf("[ERROR] [%s] 初始失败", ind)
		return 1
	}
	if success {
		log.Printf("[INFO] [%s] 初始已成功", ind)
		return 0
	}
	log.Printf("[INFO] [%s] 初始进行中", ind)

	// 事件流
	es := client.EventStream()
	eventCh, err := es.Stream(ctx, map[api.Topic][]string{api.TopicAllocation: {jobName}}, 0, &api.QueryOptions{})
	if err != nil {
		log.Printf("[WARN] 事件流失败，切换轮询: %v", err)
		eventCh = nil
	} else {
		log.Println("[INFO] 事件流已连接")
	}

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timeoutCh = time.After(time.Duration(timeout) * time.Second)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	elapsed := 0

	for {
		select {
		case <-ctx.Done():
			return 1
		case <-timeoutCh:
			log.Printf("[ERROR] 超时 %d 秒", timeout)
			return 1
		case events, ok := <-eventCh:
			if !ok {
				eventCh = nil
				log.Println("[WARN] 事件流断开，切换轮询")
				continue
			}
			for _, ev := range events.Events {
				if updateCacheFromEvent(cache, &ev, jobName, group, jobType) {
					if group != "" {
						expectedCount, _ = getTaskGroupCount(client, jobName, group)
					}
					success, failed, ind = checkStatus(cache, group, jobType, expectedCount, waitMode)
					if failed {
						log.Printf("[ERROR] [%s] 事件失败", ind)
						return 1
					}
					if success {
						log.Printf("[INFO] [%s] 事件成功", ind)
						return 0
					}
				}
			}
		case <-ticker.C:
			elapsed += 2
			if eventCh == nil {
				allocs, _ := monitoredAllocations(client, jobName, group, jobType)
				cache = make(AllocCache)
				for _, a := range allocs {
					cache[a.ID] = a
				}
				if group != "" {
					expectedCount, _ = getTaskGroupCount(client, jobName, group)
				}
			}
			success, failed, ind = checkStatus(cache, group, jobType, expectedCount, waitMode)
			if failed {
				log.Printf("[ERROR] [%s] 轮询失败", ind)
				return 1
			}
			if success {
				log.Printf("[INFO] [%s] 轮询成功", ind)
				return 0
			}
			if elapsed%10 == 0 {
				log.Printf("[INFO] [%s] 等待 %ds...", ind, elapsed)
			}
		}
	}
}

func main() {
	os.Exit(Run())
}
