package main

import (
	"database/sql"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/ipdcode/containerfs/logger"
	rp "github.com/ipdcode/containerfs/proto/rp"
	//mp "github.com/ipdcode/containerfs/proto/mp"
	//vp "github.com/ipdcode/containerfs/proto/vp"
	//"github.com/ipdcode/containerfs/utils"
	"github.com/lxmgo/config"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"io/ioutil"
	"bufio"
)

type addr struct {
	host string
	port int
	log  string
}

// RepairServerAddr
var RepairServerAddr addr

// MetaNodeAddr
var MetaNodeAddr string

// EtcdAddrs
//var EtcdAddrs []string

// Wg
var Wg sync.WaitGroup

type mysqlc struct {
	dbhost     string
	dbusername string
	dbpassword string
	dbname     string
}

var mysqlConf mysqlc

const (
	Blksize = 10 /*G*/
)

// Mutexvar g_RpcConfig RpcConfigOpts
var Mutex sync.RWMutex
var err string

// VolMgrServer
type RepairServer struct{}

// VolMgrDB
var VolMgrDB *sql.DB

func checkErr(err error) {
	if err != nil {
		logger.Error("%s", err)
	}
}

func getNeedRepairBlks() {
	var blkid int
	var port int
	var bs []int
	blks, err := VolMgrDB.Query("SELECT blkid,hostport FROM blk WHERE hostip=? and allocated=1 and disabled=0 and repair=1", RepairServerAddr.host)
	if err != nil {
                logger.Error("Get from blk table for need repair blkds in this node error:%s", err)
        }
        defer blks.Close()
        for blks.Next() {
                err = blks.Scan(&blkid,&port)
                if err != nil {
                        logger.Error("Scan db for get need repair blk error:%v", err)
                        continue
                }
		bs = append(bs,blkid)
		//Wg.Add(1)
		go repairblk(blkid,port)
        }
	logger.Debug("== Begin repair blks:%v ==",bs)
}

func repairblk(id int, port int) {
	logger.Debug("=== Begin repair blk:%v ====",id)
	var blks string
	blkgrp, err := VolMgrDB.Query("SELECT blks FROM blkgrp WHERE FIND_IN_SET(?,blks) and statu=1",id)
        if err != nil {
                logger.Error("Get from blk table for all bad node blks error:%s", err)
		//Wg.Add(-1)
                return
        }   
        defer blkgrp.Close()
	for blkgrp.Next() {
		err = blkgrp.Scan(&blks)
		if err != nil {
                        logger.Error("Scan db for get bad blk error:%v", err)
                        continue
                }
		s := strings.Split(blks,",")
		for _,v := range s[:len(s)-1]{
			bakid,_ := strconv.Atoi(v)
			if bakid != id {
				var srcip string
				var srcport int
				var disabled int
				var repair int
				blk, err := VolMgrDB.Query("SELECT hostip,hostport,disabled,repair FROM blk WHERE blkid=?",bakid)
				if err != nil {
					logger.Error("Get from blk table bakblk:%v for need repair blk:%v error:%s", bakid,id,err)
				}
				defer blk.Close()
				for blk.Next() {
					err = blk.Scan(&srcip,&srcport,&disabled,&repair)
					if err != nil {
						logger.Error("Scan db for get need repair blk:%v - bakblk:%v error:%v", id,bakid,err)
					}
				}
				if disabled == 0 && repair == 0 {
					beginRepairblk(srcip,srcport,port,bakid,id)
					break
				}
			}
		}
	}
	//Wg.Add(-1)
}

func beginRepairblk(srcip string,srcport int,dstport int, srcblkid int,dstblkid int) {
	logger.Debug("Begin repair dstip:%v-dstport:%v-dstblk:%v from srcip:%v-srcport:%v-srcblk:%v",RepairServerAddr.host,dstport,dstblkid,srcip,srcport,srcblkid)
	srcAddr := srcip + ":" + strconv.Itoa(RepairServerAddr.port)
	conn, err := grpc.Dial(srcAddr, grpc.WithInsecure())
	if err != nil {
                logger.Error("Connect Src Repair Server:%v failed : Dial to failed, reason:%v !", srcAddr,err)
               	//Wg.Add(-1)
                return
        }
	defer conn.Close()
	c := rp.NewRepairClient(conn)
	getSrcDataReq := &rp.GetSrcDataReq{
		BlkId: int32(srcblkid),
		DstBlkId: int32(dstblkid),
		SrcIp: srcip,
		SrcPort: int32(srcport),
		DstIp: RepairServerAddr.host,
		DstPort: int32(dstport),
	}
        pAck, err := c.GetSrcData(context.Background(), getSrcDataReq)
	if pAck.Ret !=0 || err != nil {
		logger.Error("Repair blk:%v from bakblk:%v fail: ack:%v--err:%v",dstblkid,srcblkid,pAck,err)
		//Wg.Add(-1)
		return
	} else {
		blk, err := VolMgrDB.Prepare("UPDATE blk SET repair=0 WHERE blkid=?")
                checkErr(err)
                defer blk.Close()
                _, err = blk.Exec(dstblkid)
                if err != nil {
                        logger.Error("The blk:%v repair complete , but update blk table repair=1 error:%s",dstblkid)
                }
	}
}

func (s *RepairServer) GetSrcData(ctx context.Context, in *rp.GetSrcDataReq) (*rp.GetSrcDataAck, error) {
	ack := rp.GetSrcDataAck{}
	srcid := in.BlkId
	srcip := in.SrcIp
	srcport := in.SrcPort
	dstid := in.DstBlkId
	dstip := in.DstIp
	dstport := in.DstPort

	var srcmp string
	var dstmp string

	disk,err := VolMgrDB.Query("SELECT mount FROM disks WHERE ip=? and port=?",srcip,srcport)
	if err != nil {
		logger.Error("Get srcblk:%v mountpath for repair dstblk:%v error:%s", srcid,dstid,err)
        }
	defer disk.Close()
	for disk.Next() {
		err = disk.Scan(&srcmp)
		if err != nil {
			logger.Error("Scan db for get need repair dstblk:%v - srcblk:%v mountpath error:%v", dstid,srcid,err)
			ack.Ret = -1
			return &ack, nil 
		}
	}

	disk,err = VolMgrDB.Query("SELECT mount FROM disks WHERE ip=? and port=?",dstip,dstport)
        if err != nil {
                logger.Error("Get dstblk:%v mountpath for repair error:%s", dstid,err)
        }
        defer disk.Close()
        for disk.Next() {
                err = disk.Scan(&dstmp)
                if err != nil {
                        logger.Error("Scan db for get need repair dstblk:%v mountpath error:%v", dstid, err)
                        ack.Ret = -1
                        return &ack, nil 
                }
        }

	srcpath := srcmp+"/block-"+strconv.FormatInt(int64(srcid), 10)
	fi, err := ioutil.ReadDir(srcpath)
	if err != nil {
		logger.Error("Read SrcBlkDir:%v error:%v",srcpath,err)
		ack.Ret = -1
		return &ack, nil
	}

	var cnt int
	ch := make(chan int)
	for _,v := range fi {
		go compare(srcpath,dstip,dstport,dstid,dstmp,v.Name(),v.Size(),ch)
	}

	for i:=0;i<len(fi);i++{
		v := <-ch
		if v == 0 || v == 1 {
			cnt += 1
		}
	}

	if cnt == len(fi) {
		logger.Debug("The DST(%v:%v-Blk:%v) have all repair complete from SRC(%v:%v-Blk:%v)",dstip,dstport,dstid,srcip,srcport,srcid)
		blk, err := VolMgrDB.Prepare("UPDATE blk SET repair=0 WHERE blkid=?")
		checkErr(err)
		defer blk.Close()
		_, err = blk.Exec(srcid)
		checkErr(err)
		blkgrp,err := VolMgrDB.Prepare("UPDATE blkgrp SET statu=0 WHERE FIND_IN_SET(?,blks)")
		checkErr(err)
		defer blkgrp.Close()
		_, err = blkgrp.Exec(srcid)
		checkErr(err)
	}
	return &ack, nil
}

func compare(srcpath string, dstip string, dstport int32,dstid int32,dstmp string, name string, size int64, ch chan int){
	srcfile := srcpath + "/"+name
	dstfile := dstmp+"/block-"+strconv.FormatInt(int64(dstid), 10)+"/"+name
	dstAddr := dstip + ":" + strconv.Itoa(RepairServerAddr.port)
	conn, err := grpc.Dial(dstAddr, grpc.WithInsecure())
	if err != nil {
                logger.Error("Connect Src Repair Server:%v failed : Dial to failed, reason:%v !", dstAddr,err)
		ch <-2
        }
        defer conn.Close()
        c := rp.NewRepairClient(conn)
        compareReq := &rp.CompareReq{
		SrcIp: RepairServerAddr.host,
		SrcFile: srcfile,
		DstFile: dstfile,
		SrcSize: size,
	}
	pAck, err := c.CompareFile(context.Background(), compareReq)
	if pAck.Ret == 0 {
		logger.Debug("This Blk chunk:%v no need repair",dstfile)
		ch <-0
	} else if pAck.Ret == 1 {
		logger.Debug("This Blk chunk:%v have repair complete",dstfile)
		ch <-1
	} else {
		logger.Debug("This Blk chunk:%v have need repair next time",dstfile)
		ch <-2
	}
}


func (s *RepairServer) CompareFile (ctx context.Context, in *rp.CompareReq) (*rp.CompareAck, error) {
	var ack rp.CompareAck
	dfile := in.DstFile
	ssize := in.SrcSize
	srcip := in.SrcIp
	sfile := in.SrcFile

	dinfo, err := os.Lstat(dfile)
	
	if os.IsNotExist(err) || ssize != dinfo.Size() {
		n := copydata(srcip,sfile,dfile,ssize)
		if n!= ssize {
			ack.Ret = 2
		} else {
			ack.Ret = 1
		}
	} else {
		ack.Ret = 0
	}

	return &ack,nil
}

func copydata(srcip string, sfile string, dfile string, ssize int64) (totalsize int64){
	addr := srcip+":"+ strconv.Itoa(RepairServerAddr.port)
	streamCopyReq := &rp.StreamCopyReq{
		SrcFile: sfile,
		DstFile: dfile,
		Ssize: ssize,
	}
	w, err := os.OpenFile(dfile, os.O_RDWR|os.O_TRUNC|os.O_CREATE,0666)
	if err != nil {
		logger.Error("Open repair blk chunk:%v error:%v",dfile,err)
		return -1
	}
	defer w.Close()
	writer := bufio.NewWriter(w)

	conn, err := grpc.Dial(addr, grpc.WithInsecure())
	if err != nil {
		logger.Error("Connect Src Repair Server:%v failed : Dial to failed, reason:%v !", addr,err)
		return -1
	}
	defer conn.Close()
	c := rp.NewRepairClient(conn)
	stream,err := c.CopyData(context.Background(), streamCopyReq)
	for {
		ack, err := stream.Recv()
		if err != nil {
			break
		}
		if ack != nil {
			if len(ack.Databuf) == 0 {
				continue
			} else {
				n, err := writer.Write(ack.Databuf)
				if err != nil || n != len(ack.Databuf) {
					return -1
				} else {
					totalsize += int64(n)
				}
			}
		} else {
			continue
		}
	}
	return totalsize
}

func (s *RepairServer) CopyData (in *rp.StreamCopyReq, stream rp.Repair_CopyDataServer) error {
	var ack rp.StreamCopyAck
	ack.DstFile = in.DstFile
	sfp := in.SrcFile
	totalsize := in.Ssize

	f, err := os.Open(sfp)
	defer f.Close()
	if err != nil {
        	return err 
        }
 
	buf := make([]byte, 1024*1024)
	bfRd := bufio.NewReader(f)
	for {
                n, err := bfRd.Read(buf)
                if err != nil {
                        return err 
                }   

                totalsize -= int64(n)
                if totalsize <= 0 { 
                        var m int64
                        m = int64(n) + totalsize
                        ack.Databuf = buf[:m]
                        if err := stream.Send(&ack); err != nil {
                                return err 
                        }   
                        break
                }   
                ack.Databuf = buf[:n]
                if err := stream.Send(&ack); err != nil {
                        return err 
                }   
        }
	return nil
}

// StartRepairService
func StartRepairService() {

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", RepairServerAddr.port))
	if err != nil {
		panic(fmt.Sprintf("Failed to listen on:%v", RepairServerAddr.port))
	}
	s := grpc.NewServer()
	rp.RegisterRepairServer(s, &RepairServer{})
	// Register reflection service on gRPC server.
	reflection.Register(s)
	if err := s.Serve(lis); err != nil {
		panic("Failed to serve")
	}
}

func init() {
	c, err := config.NewConfig(os.Args[1])
	if err != nil {
		fmt.Println("NewConfig err")
		os.Exit(1)
	}
	port, _ := c.Int("port")
	RepairServerAddr.port = port
	RepairServerAddr.log = c.String("log")
	RepairServerAddr.host = c.String("host")
	//EtcdAddrs = c.Strings("etcd::hosts")
	os.MkdirAll(RepairServerAddr.log, 0777)

	mysqlConf.dbhost = c.String("mysql::host")
	mysqlConf.dbusername = c.String("mysql::user")
	mysqlConf.dbpassword = c.String("mysql::passwd")
	mysqlConf.dbname = c.String("mysql::db")

	logger.SetConsole(true)
	logger.SetRollingFile(RepairServerAddr.log, "repair.log", 10, 100, logger.MB) //each 100M rolling
	switch level := c.String("loglevel"); level {
	case "error":
		logger.SetLevel(logger.ERROR)
	case "debug":
		logger.SetLevel(logger.DEBUG)
	case "info":
		logger.SetLevel(logger.INFO)
	default:
		logger.SetLevel(logger.ERROR)
	}

	VolMgrDB, err = sql.Open("mysql", mysqlConf.dbusername+":"+mysqlConf.dbpassword+"@tcp("+mysqlConf.dbhost+")/"+mysqlConf.dbname+"?charset=utf8")
	checkErr(err)
	err = VolMgrDB.Ping()
	checkErr(err)

}
func main() {

	//for multi-cpu scheduling
	numCPU := runtime.NumCPU()
	runtime.GOMAXPROCS(numCPU)

	defer func() {
		if err := recover(); err != nil {
			logger.Error("panic !!! :%v", err)
		}
		logger.Error("stacks:%v", string(debug.Stack()))
	}()

	//ticker := time.NewTicker(time.Second * 30)
	ticker := time.NewTicker(1)
	for {
		select {
			case <-ticker.C:
				getNeedRepairBlks()
				ticker = time.NewTicker(time.Second * 10)
		}
	}
	//go func() {
	//	for _ = range ticker.C {
	//		getNeedRepairBlks()
	//		ticker = time.NewTicker(time.Second * 10)
	//	}
	//}()
	//Wg.Wait()
	defer VolMgrDB.Close()
	StartRepairService()
}
