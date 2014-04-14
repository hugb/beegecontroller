package cluster

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"time"

	"github.com/dotcloud/docker/engine"
	dockerUtils "github.com/dotcloud/docker/utils"

	"github.com/hugb/beegecontroller/config"
	"github.com/hugb/beegecontroller/utils"
)

func ControllerJoinCluster() {
	// 获取所有controller
	getController(config.CS.JoinPoint, "controller")

	log.Println("controllers", config.CS.ClusterServer.Controller)
}

func DockerJoinCluster(eng *engine.Engine) {
	// 获取所有controller
	getController(config.CS.JoinPoint, "docker")

	log.Println("controllers", config.CS.ClusterServer.Controller)

	// 连接所有controller
	var wg sync.WaitGroup
	for address, _ := range config.CS.ClusterServer.Controller {
		wg.Add(1)
		go connectController(address, &wg)
	}
	// 等待所有连接完成
	wg.Wait()
	// todo:初始数据上报，包括镜像和容器
	reportImagesAndContainers(eng)
	log.Println("report images and containers finish.")
	// todo:docker服务器状态上报
	go reportStatus()
	// todo:事件监听上报
	if err := reportEvents(eng); err != nil {
		log.Println("Report event error:", err)
	}
	log.Println("event report finish.")
}

func reportImagesAndContainers(eng *engine.Engine) error {
	// 读取镜像列表
	log.Println("report images start.")
	imageJob := eng.Job("images")
	imageJob.Setenv("filter", "")
	imageJob.Setenv("all", "0")
	imageSrc, err := imageJob.Stdout.AddPipe()
	if err != nil {
		log.Fatalf("Create images receive pipe error:%s", err)
	}
	// 从管道读取事件数据并广播给所有controller
	go func() {
		imagesBytes, err := ioutil.ReadAll(imageSrc)
		if err != nil {
			log.Println("Read data error from pipe:", err)
		}
		images := utils.PacketByes(append(imagesBytes, " docker_images"...))
		ClusterSwitcher.Broadcast(images)
	}()
	if err := imageJob.Run(); err != nil {
		return err
	}
	// 读取容器列表
	log.Println("report containers start.")
	containerJob := eng.Job("containers")
	containerJob.Setenv("all", "1")
	containerSrc, err := containerJob.Stdout.AddPipe()
	if err != nil {
		log.Fatalf("Create containers receive pipe error:%s", err)
	}
	// 从管道读取事件数据并广播给所有controller
	go func() {
		containersBytes, err := ioutil.ReadAll(containerSrc)
		if err != nil {
			log.Println("Read data error from pipe:", err)
		}
		containers := utils.PacketByes(append(containersBytes, " docker_containers"...))
		ClusterSwitcher.Broadcast(containers)
	}()

	return containerJob.Run()
}

func reportEvents(eng *engine.Engine) error {
	job := eng.Job("events", "DockerAgent")
	// 从当前到3214080000（100年）后
	job.Setenv("since", fmt.Sprint(time.Now().Unix()))
	job.Setenv("until", fmt.Sprint(time.Now().Unix()+3214080000))
	reader, err := job.Stdout.AddPipe()
	if err != nil {
		log.Fatalf("Create event receive pipe error:%s", err)
	}
	// 从管道读取事件数据并广播给所有controller
	go func() {
		dec := json.NewDecoder(reader)
		for {
			m := &dockerUtils.JSONMessage{}
			if err := dec.Decode(m); err != nil {
				log.Printf("Error streaming events: %s", err)
				break
			}
			if b, err := json.Marshal(m); err == nil {
				// 广播
				log.Println("event:", string(b))
				content := utils.PacketByes(append(b, " docker_event"...))
				ClusterSwitcher.Broadcast(content)
			}
		}
	}()

	return job.Run()
}

func reportStatus() {
	log.Println("report status start.")
	tick := time.Tick(time.Duration(5) * time.Second)
	for {
		select {
		case <-tick:
			systemInfo, err := utils.GetSystemInfo()
			if err != nil {
				log.Println("Get system info error:", err)
			}
			systemInfoBytes, err := json.Marshal(systemInfo)
			if err != nil {
				log.Println("Encode system info error:", err)
			}
			log.Println("System info:", string(systemInfoBytes))
			data := utils.PacketByes(append(systemInfoBytes, " docker_stataus"...))
			ClusterSwitcher.Broadcast(data)
		}
	}
	log.Println("report status finish.")
}

// docker连接到controller，保持着
// todo:reconnection，断开重试
func connectController(address string, wg *sync.WaitGroup) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		panic(err)
	}

	c := &utils.Connection{
		Conn: conn,
		Src:  address,
	}
	ClusterSwitcher.register <- c

	defer func() {
		conn.Close()
		ClusterSwitcher.unregister <- c
	}()

	wg.Done()
	c.SendCommandString("docker_greetings", config.CS.ServiceAddress)

	for {
		lenght, data, err := c.Read()
		if err != nil {
			break
		}

		cmd, code, payload := utils.CmdResultDecode(lenght, data)
		log.Printf("cmd:%s,code:%s,data:%s", cmd, code, string(payload))

		if handler, exist := ClusterSwitcher.handlers[cmd]; exist {
			handler(c, payload)
		}
	}
}

// 由入口地址得到所有的controller
func getController(address, from string) {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		panic(err)
	}

	c := &utils.Connection{
		Conn: conn,
	}

	defer func() {
		conn.Close()
	}()

	if from == "docker" {
		c.WriteString(fmt.Sprintf("%s_join_cluster", from), config.CS.ServiceAddress)
	}
	if from == "controller" {
		c.WriteString(fmt.Sprintf("%s_join_cluster", from), config.CS.ClusterAddress)
	}

	lenght, data, err := c.Read()
	if err != nil {
		return
	}

	cmd, code, payload := utils.CmdResultDecode(lenght, data)
	log.Printf("cmd:%s, code:%s, payload:%s", cmd, code, string(payload))
	if code == utils.FAILURE {
		return
	}

	var controllers map[string]int64
	if err = json.Unmarshal(payload, &controllers); err != nil {
		log.Println("decode json error:", err)
	}

	for address, _ := range controllers {
		if _, exist := config.CS.ClusterServer.Controller[address]; !exist {
			config.CS.ClusterServer.Controller[address] = time.Now().Unix()
			getController(address, from)
		}
	}
}
