package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gnh123/scheduler/model"
	"github.com/gnh123/scheduler/slog"
	"github.com/gnh123/scheduler/utils"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var upgrader = websocket.Upgrader{}

// TODO, 规范下错误码

// Gate模块定位是网关
// 1.注册自己的信息至etcd中
// 2.维护lambda过来的长连接
// 3.维护管理接口，保存到数据库中
type Gate struct {
	ServerAddr   string        `clop:"short;long" usage:"server address"`
	AutoFindAddr bool          `clop:"short;long" usage:"Automatically find unused ip:port, Only takes effect when ServerAddr is empty"`
	EtcdAddr     []string      `clop:"short;long" usage:"etcd address"`
	NamePrefix   string        `clop:"long" usage:"name prfix"`
	Name         string        `clop:"short;long" usage:"The name of the gate. If it is not filled, the default is uuid"`
	Level        string        `clop:"short;long" usage:"log level"`
	LeaseTime    time.Duration `clop:"long" usage:"lease time" default:"4s"`

	*slog.Slog
	ctx context.Context
}

func (g *Gate) NodeName() string {
	return fmt.Sprintf("%s-%s", g.NamePrefix, g.Name)
}

var (
	conns         sync.Map
	defautlClient *clientv3.Client
	defaultKVC    clientv3.KV
)

func (r *Gate) init() (err error) {

	r.ctx = context.TODO()
	r.Slog = slog.New(os.Stdout).SetLevel(r.Level)
	if r.Name == "" {
		r.Name = uuid.New().String()
	}

	if r.LeaseTime < model.RuntimeKeepalive {
		r.LeaseTime = model.RuntimeKeepalive + time.Second
	}

	if defautlClient, err = utils.NewEtcdClient(r.EtcdAddr); err != nil { //初始etcd客户端
		return err
	}

	defaultKVC = clientv3.NewKV(defautlClient) // 内置自动重试的逻辑
	return nil
}

func (r *Gate) getAddress() string {
	if r.ServerAddr != "" {
		return r.ServerAddr
	}

	if r.AutoFindAddr {
		return utils.GetUnusedAddr()
	}
	return ""
}

// gate的地址
// model.GateNodePrefix 注册到/scheduler/gate/node/gate_name
func (r *Gate) registerGateNode() error {
	addr := r.getAddress()
	if addr == "" {
		panic("The service startup address is empty, please set -s ip:port")
	}

	leaseID, err := utils.NewLeaseWithKeepalive(r.ctx, r.Slog, defautlClient, r.LeaseTime)
	if err != nil {
		return err
	}

	// 注册自己的节点信息
	_, err = defautlClient.Put(r.ctx, model.FullGateNodePath(r.NodeName()), r.ServerAddr, clientv3.WithLease(leaseID))
	return err
}

// 注册runtime节点，并负责节点lease的续期
func (r *Gate) registerRuntimeWithKeepalive(runtimeName string, keepalive chan bool) error {
	lease, leaseID, err := utils.NewLease(r.ctx, r.Slog, defautlClient, r.LeaseTime)
	if err != nil {
		r.Error().Msgf("registerRuntimeWithKeepalive.NewLease fail:%s\n", err)
		return err
	}

	// 注册自己的节点信息
	_, err = defautlClient.Put(r.ctx, model.FullRuntimeNodePath(r.NodeName()), r.ServerAddr, clientv3.WithLease(leaseID))
	for range keepalive {
		lease.KeepAliveOnce(r.ctx, leaseID)
	}
	return nil
}

func (r *Gate) stream(c *gin.Context) {

	w := c.Writer
	req := c.Request

	con, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.Error().Msgf("upgrade:", err)
		return
	}
	defer con.Close()

	first := true
	keepalive := make(chan bool)
	for {
		// 读取心跳
		req := model.Whoami{}
		err := con.ReadJSON(&req)
		if err != nil {
			log.Println("read:", err)
			break
		}

		if first {
			go r.registerRuntimeWithKeepalive(req.Name, keepalive)
			first = false
		} else {
			keepalive <- true
		}

		// TODO
		/*
			err = con.WriteJSON(mt, map[])
			if err != nil {
				log.Println("write:", err)
				break
			}
		*/
	}
}

func (r *Gate) ok(c *gin.Context, msg string) {
	r.Debug().Caller(1).Msg(msg)
	c.JSON(200, gin.H{"code": 0, "message": ""})
}

// 简单的包装函数
func (r *Gate) error(c *gin.Context, code int, format string, a ...any) {

	msg := fmt.Sprintf(format, a...)
	r.Error().Caller(1).Msg(msg)
	c.JSON(500, gin.H{"code": code, "message": msg})
}

// 把task信息保存至etcd
func (r *Gate) createTask(c *gin.Context) {
	var req model.Param
	err := c.ShouldBind(&req)
	if err != nil {
		r.error(c, 500, "createTask:%v", err)
		return
	}

	globalTaskName := model.FullGlobalTaskPath(req.Executer.TaskName)
	globalTaskStateName := model.FullGlobalTaskStatePath(req.Executer.TaskName)

	// 先get，如果有值直接返回
	rsp, err := defaultKVC.Get(r.ctx, globalTaskName, clientv3.WithKeysOnly())
	if len(rsp.Kvs) > 0 {
		r.error(c, 500, "duplicate creation:%s", globalTaskName)
		return
	}

	all, err := json.Marshal(req)
	if err != nil {
		r.error(c, 500, "marshal req:%v", err)
		return
	}

	txn := defaultKVC.Txn(r.ctx)
	txn.If(clientv3.Compare(clientv3.CreateRevision(globalTaskName), "=", 0)).
		Then(
			clientv3.OpPut(globalTaskName, string(all)),
			clientv3.OpPut(globalTaskStateName, model.CanRun),
		).Else()

	txnRsp, err := txn.Commit()
	if err != nil {
		r.error(c, 500, "事务执行失败err :%v", err)
		return
	}

	if !txnRsp.Succeeded {
		r.error(c, 500, "事务失败")
		return
	}

	r.ok(c, "createTask 执行成功") //返回正确业务码
}

// 删除etcd里面task信息，也直接下发命令更新runtime里面信息
func (r *Gate) deleteTask(c *gin.Context) {

}

// 更新etcd里面的task信息，也下发命令更新runtime里面信息
func (r *Gate) updateTask(c *gin.Context) {

}

// 更新etcd里面的task信息，置为静止，下发命令取消正在执行中的task
func (r *Gate) stopTask(c *gin.Context) {

}

// 该模块入口函数
func (r *Gate) SubMain() {
	if err := r.init(); err != nil {
		// init是必须要满足的条件，不成功直接panic
		panic(err.Error())
	}

	go r.registerGateNode()

	g := gin.New()
	g.GET(model.TASK_STREAM_URL, r.stream) //流式接口，主动推送任务至runtime
	g.POST(model.TASK_CREATE_URL, r.createTask)
	g.DELETE(model.TASK_DELETE_URL, r.deleteTask)
	g.PUT(model.TASK_UPDATE_URL, r.updateTask)
	g.POST(model.TASK_STOP_URL, r.stopTask)

	g.Run()
}
