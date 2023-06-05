package main

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

const (
	RedisTopPingKey      = "top_pings"
	RedisHyperLogLogKey  = "hyperloglog"
	RedisExpirationTime  = 300 * time.Second
	CookieExpirationTime = 300
	MaxPingPerUser       = 2
	PingRateLimit        = 60
	TopPingCount         = 10
)

var (
	db          *gorm.DB
	router      *gin.Engine
	redisClient *redis.Client
	mu          sync.Mutex
)

func init() {
	initDatabase()
	initRedis()
	initRouter()
	mu = sync.Mutex{}
}

// initDatabase initializes the database
func initDatabase() {
	var err error
	db, err = gorm.Open(mysql.New(mysql.Config{
		DSN:                       "quangmx:2511@tcp(127.0.0.1:3306)/engineerpro?charset=utf8mb4&parseTime=True&loc=Local",
		DefaultStringSize:         256,
		DisableDatetimePrecision:  true,
		DontSupportRenameIndex:    true,
		SkipInitializeWithVersion: false,
	}), &gorm.Config{
		SkipDefaultTransaction: true,
	})
	if err != nil {
		fmt.Println("Can not connect to db:", err)
		return
	}
}

// initRedis initializes the Redis instance
func initRedis() {
	redisClient = redis.NewClient(&redis.Options{})
	if redisClient == nil {
		fmt.Println("Can not initialize redis")
		return
	}
}

// initRouter initializes the gin router
func initRouter() {
	router = gin.Default()
}

func main() {
	// Declare /login API
	router.POST("/login", handleLogin)

	// Declare /ping API
	router.GET("/ping", handlePing)

	// Delcare /top API
	router.GET("/top", handleTop)

	// Declare /count API
	router.GET("/count", handleCount)

	// Start the web server
	err := router.Run(":8080")
	if err != nil {
		fmt.Println(err)
	}
}

type Auth struct {
	ID       int    `gorm:"primaryKey" json:"id"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin logs user in if valid and save sessionID in redis
func handleLogin(c *gin.Context) {
	// Get username and password
	username := c.PostForm("username")
	password := c.PostForm("password")

	// Check validity of username and password
	var auth Auth
	db.Raw("SELECT id from User where username = ? and password = ?", username, password).Scan(&auth)
	if auth.ID == 0 {
		c.IndentedJSON(http.StatusUnauthorized, gin.H{"message": "Wrong username or password"})
		return
	}

	// If logged in, set a sessionID for this session
	sessionID := uuid.New().String()

	// Save current sessionID and username in Redis
	err := redisClient.Set(redisClient.Context(), sessionID, username, RedisExpirationTime).Err()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
		})
		return
	}

	// Set sessionID cookie
	c.SetCookie("sessionID", sessionID, CookieExpirationTime, "/", "localhost", false, true)

	c.IndentedJSON(http.StatusOK, gin.H{"message": "Log in successfully!", "sessionID": sessionID})
}

// handlePing allows just one user calls at a time
func handlePing(c *gin.Context) {
	// Acquire the lock
	mu.Lock()
	defer mu.Unlock()

	sessionID, err := c.Cookie("sessionID")
	if err != nil {
		c.IndentedJSON(http.StatusUnauthorized, gin.H{
			"message": err.Error(),
		})
		return
	}

	username, err := redisClient.Get(redisClient.Context(), sessionID).Result()
	if err != nil {
		c.IndentedJSON(http.StatusUnauthorized, gin.H{
			"message": err.Error(),
		})
		return
	}

	// Return if can not find sessionID or username
	if sessionID == "" || username == "" {
		c.IndentedJSON(http.StatusUnauthorized, gin.H{"status": "Unauthorized"})
		return
	}

	// Check if the user has exceeded the rate limit for /ping API
	if !canMakePing(username) {
		c.IndentedJSON(http.StatusTooManyRequests, gin.H{"message": "Rate limit exceeded"})
		return
	}

	// Increase the counter for the user's /ping calls
	increaseCounter(username)

	// Simulate work inside /ping API
	time.Sleep(3 * time.Second)

	c.IndentedJSON(http.StatusOK, gin.H{"message": "Ping succeeded."})
}

// đếm số lượng lần 1 người gọi api /ping
func increaseCounter(username string) {
	totalPing, _ := redisClient.ZScore(redisClient.Context(), RedisTopPingKey, username).Result()
	err := redisClient.ZAdd(redisClient.Context(), RedisTopPingKey, &redis.Z{Score: totalPing + 1, Member: username}).Err()
	if err != nil {
		panic(err)
	}

	err = redisClient.PFAdd(redisClient.Context(), RedisHyperLogLogKey, username).Err()
	if err != nil {
		panic(err)
	}
}

func canMakePing(username string) bool {
	// Create a map to save ping of each user -> this map is on redis -> can scale up
	pingID := "ping-" + username
	pingInfo, _ := redisClient.HGetAll(redisClient.Context(), pingID).Result()

	// If pingInfo is empty then create new pingInfo
	if len(pingInfo) == 0 {
		err := setPingInfo(pingID, 0, int(time.Now().Unix()))
		if err != nil {
			panic(err)
		}
		return true
	}

	currPingTime := time.Now().Unix()
	blockTime, _ := strconv.ParseInt(pingInfo["blockTime"], 10, 32)
	lastPingTime, _ := strconv.ParseInt(pingInfo["lastPingTime"], 10, 32)

	if int(currPingTime)-int(lastPingTime) > int(blockTime) {
		newBlockTime := math.Max(float64(0), float64(int(lastPingTime)+int(PingRateLimit)-int(currPingTime)))
		err := setPingInfo(pingID, int(newBlockTime), int(currPingTime))
		if err != nil {
			panic(err)
		}
		return true
	}
	return false
}

func setPingInfo(pingID string, blockTime, currPingTime int) error {
	pingRecord := map[string]int{"blockTime": blockTime, "lastPingTime": currPingTime}
	for k, v := range pingRecord {
		err := redisClient.HSet(redisClient.Context(), pingID, k, v).Err()
		if err != nil {
			return err
		}
	}
	return nil
}

// handleTop retrieves the top 10 callers of /ping API
func handleTop(c *gin.Context) {
	topUsers, err := redisClient.ZRevRangeWithScores(redisClient.Context(), RedisTopPingKey, 0, TopPingCount-1).Result()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve top users"})
		return
	}

	c.IndentedJSON(http.StatusOK, gin.H{"topUsers": topUsers})
}

// handleCount retrieves number of users called /ping
func handleCount(c *gin.Context) {
	count, err := redisClient.PFCount(redisClient.Context(), RedisHyperLogLogKey).Result()
	if err != nil {
		panic(err)
	}
	c.IndentedJSON(http.StatusOK, gin.H{"Number of /ping users": count})
}
