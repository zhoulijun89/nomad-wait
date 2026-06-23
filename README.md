# Nomad Wait

等待 HashiCorp Nomad Job 的 Allocation 达到健康或完成状态。

工具优先使用 Nomad Event Stream 监听状态；事件流不可用时自动切换为每 2 秒轮询。

对于同一个 `TaskGroup + Index`，只检查最新的 Allocation。Job 重调度后，旧的 `lost` Allocation 不会阻塞新的健康 Allocation。

## 用法

```shell
nomad-wait [参数] <作业名称>
```

示例：

```shell
nomad-wait -address http://127.0.0.1:4646 -group mysql -mode all -timeout 120 example-job
```

查看构建信息：

```shell
nomad-wait -v
```

## 参数

- `-v`：打印编译时间和 Git commit hash 后退出。
- `-address`：Nomad 服务地址。默认读取 `NOMAD_ADDR`，未设置时使用 `http://127.0.0.1:4646`。
- `-t`、`-timeout`：等待超时时间，单位为秒。`0` 表示永不超时，默认读取 `NOMAD_JOB_TIMEOUT`，未设置时为 `60`。
- `-group`：只等待指定 Task Group。默认读取 `NOMAD_TASK_GROUP`；未设置时检查 Job 的所有 Task Group。
- `-token`：Nomad ACL Token。默认读取 `NOMAD_TOKEN`。
- `-mode`：等待模式，默认为 `any`。
  - `any`：任意一个 Allocation 健康或完成即成功。
  - `all`：所有预期 Allocation 均健康或完成才成功。

命令行参数会覆盖对应的环境变量。

## 环境变量

- `NOMAD_ADDR`：Nomad 服务地址。
- `NOMAD_JOB_TIMEOUT`：等待超时时间，单位为秒；`0` 表示永不超时。
- `NOMAD_TASK_GROUP`：要等待的 Task Group。
- `NOMAD_TOKEN`：Nomad ACL Token。
- `NOMAD_LOG_LEVEL`：日志格式配置；设置为 `debug` 时会附带源码位置，默认为 `info`。

## 状态判断

- Service Job：Allocation 达到 `healthy` 时视为成功。
- Batch Job：Allocation 达到 `complete` 时视为成功。
- 返回码 `0`：等待成功。
- 返回码 `1`：Allocation 失败、等待超时、参数错误、请求失败或收到中断信号。
