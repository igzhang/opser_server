package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}
var CmdNothingID uint = 0
var db *gorm.DB
var (
	jobStateToSchedule    = 0
	jobStateExecSucceeded = 1
	jobStateExecFailed    = 2
)
var OnlineConn = make(map[string]time.Time)

type Cmd struct {
	ID      uint   `json:"id"`
	Context string `json:"context"`
}

type CmdResult struct {
	ID        uint   `json:"id"`
	IsSuccess bool   `json:"is_success"`
	Context   string `json:"stdout"`
	Hostname  string `json:"hostname"`
}

type CmdDB struct {
	gorm.Model
	Shell    string `gorm:"not null"`
	State    int    `gorm:"index:idx_query,not null"`
	Result   string
	Hostname string `gorm:"index:idx_query,not null"`
}

type ECSTagDB struct {
	gorm.Model
	Hostname string `gorm:"unique"`
	Tagname  string `gorm:"unique"`
}

type CmdRequest struct {
	Tagname string `form:"ecs"`
	Cmd     string `form:"cmd"`
	Token   string `form:"token"`
}

func QueryNextJob(hostname string) *Cmd {
	var cmdDB CmdDB
	if result := db.Where("hostname = ? and state = ?", hostname, jobStateToSchedule).First(&cmdDB); errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return &Cmd{
			ID: CmdNothingID,
		}
	}
	return &Cmd{
		ID:      cmdDB.ID,
		Context: cmdDB.Shell,
	}
}

func RecordJobResult(result *CmdResult) error {
	state := jobStateExecSucceeded
	if !result.IsSuccess {
		state = jobStateExecFailed
	}
	if res := db.Model(&CmdDB{}).Where("id = ?", result.ID).Updates(CmdDB{State: state, Result: result.Context}); res.Error != nil {
		log.Println(res.Error)
		return res.Error
	}
	return nil
}

func handleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer conn.Close()
	conn.SetPingHandler(func(hostname string) error {
		OnlineConn[hostname] = time.Now()
		return nil
	})

	for {
		var cmdResult CmdResult
		if err := conn.ReadJSON(&cmdResult); err != nil {
			log.Println(err)
			return
		}

		// query todo jobs
		if cmdResult.ID == CmdNothingID {
			cmd := QueryNextJob(cmdResult.Hostname)
			if err := conn.WriteJSON(cmd); err != nil {
				log.Println(err)
				return
			}
			continue
		}

		// record job result
		RecordJobResult(&cmdResult)
	}
}

func InitDB() error {
	var err error

	dsn := os.Getenv("DSN")
	if len(dsn) == 0 {
		return errors.New("env dsn not found")
	}

	dbLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags), // io writer
		logger.Config{
			IgnoreRecordNotFoundError: true, // Ignore ErrRecordNotFound error for logger
		},
	)
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: dbLogger})
	if err != nil {
		return err
	}

	// Migrate the schema
	if err := db.AutoMigrate(&CmdDB{}, &ECSTagDB{}); err != nil {
		return err
	}

	return nil
}

func handleSendCmdDirect(c *gin.Context) {
	var cmdRequest CmdRequest
	if err := c.ShouldBind(&cmdRequest); err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	token := os.Getenv("TOKEN")
	if token != cmdRequest.Token {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	var ecsTagDB ECSTagDB
	if result := db.Where("tagname = ?", cmdRequest.Tagname).First(&ecsTagDB); errors.Is(result.Error, gorm.ErrRecordNotFound) {
		c.AbortWithError(http.StatusBadRequest, errors.New("tagname not found"))
		return
	}

	job := CmdDB{Shell: cmdRequest.Cmd, State: jobStateToSchedule, Hostname: ecsTagDB.Hostname}
	if res := db.Create(&job); res.Error != nil {
		c.AbortWithError(http.StatusInternalServerError, res.Error)
	}

	c.String(http.StatusOK, "get it")
}

func handleOnlineConn(c *gin.Context) {
	endTime := time.Now().Add(-30 * time.Second)
	unactiveConn := []string{}

	for host, connTime := range OnlineConn {
		if connTime.Before(endTime) {
			unactiveConn = append(unactiveConn, host)
		}
	}

	for _, v := range unactiveConn {
		delete(OnlineConn, v)
	}

	c.JSON(http.StatusOK, gin.H{
		"count": len(OnlineConn),
		"list":  OnlineConn,
	})
}

func main() {
	log.Println("start connect to database")
	if err := InitDB(); err != nil {
		log.Fatalf("init db error: %v", err)
	}

	log.Println("start webserver")
	r := gin.Default()
	r.GET("/ws", handleWebSocket)
	r.GET("/online", handleOnlineConn)
	r.POST("/cmd", handleSendCmdDirect)

	r.Run()
}
