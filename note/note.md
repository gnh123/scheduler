## 设计笔记

## 调度
* mjobs: 负责分配任务
* gate: 负责对接runtime,以及web的crud, 最后是下发任务。当watch本gate有新的任务时，把任务下发到runtime
* runtime: 负责执行任务, runtime长连接到gate，这样runtime和gate分布式部署也能执行任务

## 数据结构
* 全局队列: 所有crud的任务都会先进全局队列
* 本地队列: 所有的任务都是以runtime的为区别。里面有需要执行的任务

## 异常
如何保证任务任务都在执行中?
1. 如果某个runtime挂了怎么办?
mjobs watch runtime节点名，如果runtime挂了就把runtime下面的任务重新分配

2. 如果集群重启了怎么办?
先随机等待一段时间。runtime和gate服务都启动就遍历本地任务列表，分配时加分布式锁。然后这个全局任务是running，
并且本地任务没有这个runtime，任务就重新分配

3. 寻检
寻栓是通过抽样的方式，找到应该执行的任务确没有执行，提升整个集群的任务可用性