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

// 定义分配状态的常量，用于判断 allocation 的健康或完成状态
const (
	STATUS_HEALTHY   = "healthy"   // 健康状态
	STATUS_UNHEALTHY = "unhealthy" // 不健康状态
	STATUS_COMPLETE  = "complete"  // 批处理作业完成状态
	STATUS_FAILED    = "failed"    // 失败状态
	STATUS_RUNNING   = "running"   // 运行中状态
)

// Allocation 结构体，扩展 Nomad 的 AllocationListStub，增加索引字段
type Allocation struct {
	api.AllocationListStub
	Index int // 从分配名称解析得到的索引
}

// AllocCache 用于缓存分配状态，便于事件更新和检查
type AllocCache map[string]*Allocation

// findAllocation 在分配列表中查找特定任务组和索引的分配
// 参数:
//   - allocs: 分配列表
//   - taskGroup: 任务组名称
//   - index: 分配索引
// 返回:
//   - 匹配的分配指针，或 nil 如果未找到
func findAllocation(allocs []*Allocation, taskGroup string, index int) *Allocation {
	for _, alloc := range allocs {
		if alloc.TaskGroup == taskGroup && alloc.Index == index {
			return alloc
		}
	}
	return nil
}

// stringFromEnv 从环境变量读取字符串值，若未设置或为空则返回默认值
// 参数:
//   - key: 环境变量名
//   - def: 默认值
// 返回:
//   - 环境变量值或默认值
func stringFromEnv(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	return def
}

// intFromEnv 从环境变量读取整数值，若解析失败则返回错误
// 参数:
//   - key: 环境变量名
//   - def: 默认值
// 返回:
//   - 整数值和可能的错误
func intFromEnv(key string, def int) (int, error) {
	if v := os.Getenv(key); v != "" {
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return def, fmt.Errorf("错误解析环境变量 %s: %v", key, err)
		}
		return i, nil
	}
	return def, nil
}

// jobNameFromArgs 解析命令行参数以获取作业名称
// 参数:
//   - args: 命令行参数列表
// 返回:
//   - 作业名称和可能的错误
func jobNameFromArgs(args []string) (string, error) {
	if len(args) == 0 || len(args) > 1 {
		return "", errors.New("参数数量不正确")
	}
	return args[0], nil
}

// parseAllocationName 解析分配名称，提取作业ID、任务组和索引
// 分配名称格式: <job>.<taskgroup>[<index>]
// 参数:
//   - name: 分配名称
// 返回:
//   - 作业ID、任务组名称、索引和可能的错误
func parseAllocationName(name string) (string, string, int, error) {
	nameRegexp := regexp.MustCompile(`([\w-]+)\.([\w-]+)\[(\d+)\]`)
	match := nameRegexp.FindStringSubmatch(name)
	if len(match) == 0 {
		return "", "", -1, fmt.Errorf("无法解析分配名称 %s", name)
	}

	index, err := strconv.Atoi(match[3])
	if err != nil {
		return "", "", -1, fmt.Errorf("无法解析分配名称 %s: %v", name, err)
	}

	return match[1], match[2], index, nil
}

// monitoredAllocations 获取匹配作业和任务组的分配列表（用于初始加载或轮询）
// 参数:
//   - client: Nomad API 客户端
//   - jobName: 作业名称
//   - group: 任务组名称（可选，空字符串表示所有组）
// 返回:
//   - 分配列表和可能的错误
func monitoredAllocations(client *api.Client, jobName, group string) ([]*Allocation, error) {
	var result []*Allocation
	q := api.QueryOptions{}

	// 获取分配列表（支持分页）
	var nextToken string
	for {
		allocs, meta, err := client.Allocations().List(&q)
		if err != nil {
			return nil, fmt.Errorf("无法从 Nomad 服务器获取分配列表: %v", err)
		}

		for _, a := range allocs {
			if a.JobID != jobName {
				continue
			}
			if group != "" && a.TaskGroup != group {
				continue
			}

			_, _, index, err := parseAllocationName(a.Name)
			if err != nil {
				return nil, err
			}

			if alloc := findAllocation(result, a.TaskGroup, index); alloc != nil {
				if alloc.CreateTime < a.CreateTime {
					alloc = &Allocation{*a, index}
				}
			} else {
				result = append(result, &Allocation{*a, index})
			}
		}

		if meta.NextToken == "" {
			break
		}
		nextToken = meta.NextToken
		q.NextToken = nextToken
	}

	return result, nil
}

// allocationStatus 获取分配的状态
// 参数:
//   - alloc: 分配对象
// 返回:
//   - 状态字符串（healthy, unhealthy, complete, failed 等）
func allocationStatus(alloc *Allocation) string {
	human_readable := map[bool]string{true: STATUS_HEALTHY, false: STATUS_UNHEALTHY}

	if alloc.ClientStatus == STATUS_RUNNING && alloc.DeploymentStatus != nil {
		return human_readable[*alloc.DeploymentStatus.Healthy]
	}

	return alloc.ClientStatus
}

// checkStatus 检查缓存中的分配是否达到目标状态
// 参数:
//   - cache: 分配缓存
//   - targetGroup: 目标任务组（空表示所有）
//   - acceptableStatus: 目标状态
// 返回:
//   - successful, failed, indicator 字符串
func checkStatus(cache AllocCache, targetGroup, acceptableStatus string) (bool, bool, string) {
	var indicator string
	successful := true
	failed := false

	for _, a := range cache {
		if targetGroup != "" && a.TaskGroup != targetGroup {
			continue
		}

		status := allocationStatus(a)
		log.Printf("[DEBUG] 检查分配 %s (TaskGroup: %s, Index: %d): 状态 %s", a.ID, a.TaskGroup, a.Index, status)

		if status == STATUS_FAILED {
			failed = true
			indicator += "!"
			successful = false
			continue
		}

		if status == acceptableStatus {
			indicator += "+"
			continue
		}

		indicator += "-"
		successful = false
	}

	return successful, failed, indicator
}

// updateCacheFromEvent 从事件更新分配缓存
// 参数:
//   - cache: 分配缓存
//   - ev: Nomad 事件
//   - jobName: 作业名称
//   - targetGroup: 目标任务组
// 返回:
//   - 是否更新了缓存
func updateCacheFromEvent(cache AllocCache, ev *api.Event, jobName, targetGroup string) bool {
	if ev.Type != "Allocation" { // 修正：使用字符串比较
		return false
	}

	// 解码事件 payload 为 Allocation
	alloc, err := ev.Allocation()
	if err != nil {
		log.Printf("[ERROR] 解码事件 payload 失败: %v", err)
		return false
	}

	if alloc == nil || alloc.JobID != jobName {
		return false
	}

	if targetGroup != "" && alloc.TaskGroup != targetGroup {
		return false
	}

	// 更新或添加缓存
	newAlloc := &Allocation{AllocationListStub: api.AllocationListStub{
		ID:              alloc.ID,
		JobID:           alloc.JobID,
		TaskGroup:       alloc.TaskGroup,
		ClientStatus:    alloc.ClientStatus,
		DeploymentStatus: alloc.DeploymentStatus,
		CreateTime:      alloc.CreateTime,
	}}
	_, _, index, err := parseAllocationName(alloc.Name)
	if err == nil {
		newAlloc.Index = index
	}
	cache[alloc.ID] = newAlloc

	log.Printf("[DEBUG] 更新分配缓存: ID=%s, TaskGroup=%s, 新状态=%s", alloc.ID, alloc.TaskGroup, alloc.ClientStatus)
	return true
}

// setupLogger 设置日志级别
// 支持环境变量 NOMAD_LOG_LEVEL（debug/info/error，默认 info）
func setupLogger() {
	level := stringFromEnv("NOMAD_LOG_LEVEL", "info")
	switch strings.ToLower(level) {
	case "debug":
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	case "error":
		log.SetFlags(log.LstdFlags)
	default:
		log.SetFlags(log.LstdFlags)
	}
	log.Printf("[INFO] 日志级别设置为 %s", level)
}

// Run 主逻辑函数，执行等待作业或任务组健康状态的逻辑（优先事件流，回退轮询）
// 返回:
//   - 退出码（0 表示成功，1 表示失败）
func Run() int {
	setupLogger() // 初始化日志

	var timeout int
	var group string
	var token string
	var err error

	// 定义命令行标志的帮助文本
	addressHelpText := `
Nomad 服务器地址。
如果设置了 NOMAD_ADDR 环境变量，将被覆盖。
默认值 = http://127.0.0.1:4646 
	`
	timeoutHelpText := `
等待超时时间（秒，0 表示永不超时）。
如果设置了 NOMAD_JOB_TIMEOUT 环境变量，将被覆盖。
默认值 = 0（无限等待）
	`
	groupHelpText := `
要等待健康状态的特定任务组（可选）。
如果未设置，等待作业中所有任务组。
如果设置了 NOMAD_TASK_GROUP 环境变量，将被覆盖。
	`
	tokenHelpText := `
Nomad ACL 认证令牌。
如果设置了 NOMAD_TOKEN 环境变量，将被覆盖。
	`

	nomadConfig := api.DefaultConfig()

	// 定义命令行标志
	flag.StringVar(&nomadConfig.Address, "address", stringFromEnv("NOMAD_ADDR", "http://127.0.0.1:4646"), strings.TrimSpace(addressHelpText))
	flag.IntVar(&timeout, "timeout", 0, strings.TrimSpace(timeoutHelpText)) // 默认永不超时
	flag.IntVar(&timeout, "t", 0, strings.TrimSpace(timeoutHelpText))
	flag.StringVar(&group, "group", stringFromEnv("NOMAD_TASK_GROUP", ""), strings.TrimSpace(groupHelpText))
	flag.StringVar(&token, "token", stringFromEnv("NOMAD_TOKEN", ""), strings.TrimSpace(tokenHelpText))
	flag.Parse()

	// 覆盖环境变量到 timeout
	if envTimeout, err := intFromEnv("NOMAD_JOB_TIMEOUT", timeout); err == nil {
		timeout = envTimeout
	} else {
		log.Printf("[ERROR] %v", err)
		return 1
	}

	// 解析作业名称
	jobName, err := jobNameFromArgs(flag.Args())
	if err != nil {
		log.Printf("[ERROR] %v", err)
		fmt.Printf("用法: %s [参数] <作业名称>\n", os.Args[0])
		flag.PrintDefaults()
		return 1
	}

	// 设置 ACL 令牌
	nomadConfig.SecretID = token
	client, err := api.NewClient(nomadConfig)
	if err != nil {
		log.Printf("[ERROR] 创建 Nomad 客户端失败: %v", err)
		return 1
	}

	// 打印启动信息
	groupMsg := "所有任务组"
	if group != "" {
		groupMsg = fmt.Sprintf("任务组 '%s'", group)
	}
	if timeout == 0 {
		log.Printf("[INFO] 无限期等待 Nomad 作业 '%s' (%s) - 尝试事件流监控", jobName, groupMsg)
	} else {
		log.Printf("[INFO] 等待 Nomad 作业 '%s' (%s) %d 秒 - 尝试事件流监控", jobName, groupMsg, timeout)
	}

	// 创建上下文以支持取消
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 处理信号（如 Ctrl+C）
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Printf("[INFO] 接收到中断信号，退出")
		cancel()
	}()

	// 步骤 1: 初始状态检查
	log.Printf("[INFO] 执行初始分配状态检查...")
	allocs, err := monitoredAllocations(client, jobName, group)
	if err != nil {
		log.Printf("[ERROR] 初始分配查询失败: %v", err)
		return 1
	}

	if len(allocs) == 0 {
		msg := fmt.Sprintf("未找到作业 '%s' 的分配", jobName)
		if group != "" {
			msg += fmt.Sprintf(" 在任务组 '%s' 中", group)
		}
		log.Printf("[ERROR] %s", msg)
		return 1
	}

	// 构建初始缓存
	cache := make(AllocCache)
	for _, a := range allocs {
		cache[a.ID] = a
	}
	log.Printf("[INFO] 初始缓存加载 %d 个分配", len(cache))

	// 初始检查
	acceptableStatus := STATUS_HEALTHY
	if allocs[0].JobType == "batch" {
		acceptableStatus = STATUS_COMPLETE
		log.Printf("[INFO] 检测到 batch 作业，使用目标状态: %s", acceptableStatus)
	}

	successful, failed, indicator := checkStatus(cache, group, acceptableStatus)
	if failed {
		log.Printf("[ERROR] [%s] 初始检查: 分配失败", indicator)
		return 1
	}
	if successful {
		log.Printf("[INFO] [%s] 初始检查: 分配已成功", indicator)
		return 0
	}
	log.Printf("[INFO] [%s] 初始检查: 分配进行中", indicator)

	// 步骤 2: 尝试订阅事件流
	var eventChan <-chan *api.Event
	const maxRetries = 3
	for attempt := 1; attempt <= maxRetries; attempt++ {
		events := client.EventStream() // 修正：使用 EventStream
		var err error
		eventChan, err = events.Stream(ctx, map[api.Topic][]string{api.Topic("Allocation"): {jobName}}, 0, &api.QueryOptions{})
		if err == nil {
			log.Printf("[INFO] 事件流订阅成功，等待相关事件...")
			break
		}
		log.Printf("[WARN] 事件流订阅失败 (尝试 %d/%d): %v", attempt, maxRetries, err)
		if attempt == maxRetries {
			log.Printf("[ERROR] 事件流订阅失败，切换到轮询模式")
			eventChan = nil
		}
		time.Sleep(time.Second * time.Duration(attempt))
	}

	// 超时通道
	var timeoutChan <-chan time.Time
	if timeout > 0 {
		timeoutChan = time.After(time.Duration(timeout) * time.Second)
	}

	// 主循环：优先事件流，失败则轮询
	var elapsed int
	ticker := time.NewTicker(2 * time.Second) // 轮询间隔 2 秒
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("[INFO] 上下文取消，退出")
			return 1
		case <-timeoutChan:
			if timeout > 0 {
				log.Printf("[ERROR] 事件等待超时 (%d 秒)", timeout)
				return 1
			}
		case ev, ok := <-eventChan:
			if !ok && eventChan != nil {
				log.Printf("[ERROR] 事件通道关闭，切换到轮询模式")
				eventChan = nil
			}
			if ok && ev != nil {
				if updated := updateCacheFromEvent(cache, ev, jobName, group); updated {
					alloc, _ := ev.Allocation() // 修正：从 ev.Allocation 获取 ID
					log.Printf("[DEBUG] 接收事件: Topic=%s, AllocID=%s, Type=%s", ev.Type, alloc.ID, ev.Type)

					successful, failed, indicator = checkStatus(cache, group, acceptableStatus)
					if failed {
						log.Printf("[ERROR] [%s] 事件触发: 分配失败 (AllocID: %s)", indicator, alloc.ID)
						return 1
					}
					if successful {
						log.Printf("[INFO] [%s] 事件触发: 分配成功 (AllocID: %s)", indicator, alloc.ID)
						return 0
					}
					log.Printf("[INFO] [%s] 事件更新: 分配进行中 (AllocID: %s)", indicator, alloc.ID)
				}
			}
		case <-ticker.C:
			elapsed += 2
			// 如果事件流不可用，使用轮询
			if eventChan == nil {
				allocs, err := monitoredAllocations(client, jobName, group)
				if err != nil {
					log.Printf("[ERROR] 轮询分配列表失败: %v", err)
					return 1
				}
				cache = make(AllocCache)
				for _, a := range allocs {
					cache[a.ID] = a
				}
				log.Printf("[DEBUG] 轮询更新缓存: %d 个分配", len(cache))
			}

			successful, failed, indicator := checkStatus(cache, group, acceptableStatus)
			if failed {
				log.Printf("[ERROR] [%s] 进度检查: 分配失败", indicator)
				return 1
			}
			if successful {
				log.Printf("[INFO] [%s] 进度检查: 分配成功", indicator)
				return 0
			}
			if elapsed%10 == 0 {
				log.Printf("[INFO] [%s] 等待中，耗时 %ds (缓存大小: %d)", indicator, elapsed, len(cache))
			}
		}
	}
}

// main 入口函数，调用 Run 并设置退出码
func main() {
	os.Exit(Run())
}
