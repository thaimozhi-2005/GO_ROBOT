package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	tele "gopkg.in/telebot.v3"
)

// Database Models
type Admin struct {
	ID         uint      `gorm:"primaryKey"`
	TelegramID int64     `gorm:"unique;not null"`
	Username   string    `gorm:"type:varchar(100)"`
	JoinedAt   time.Time `gorm:"autoCreateTime"`
}

type Bot struct {
	ID              uint      `gorm:"primaryKey"`
	BotUsername     string    `gorm:"type:varchar(100);not null"`
	BotURL          string    `gorm:"type:varchar(500)"` // HTTP URL to ping
	IntervalMinutes int       `gorm:"default:5"`
	LastPing        time.Time
	Status          string    `gorm:"type:varchar(20);default:'Unknown'"`
	AddedBy         int64     `gorm:"not null"`
	CreatedAt       time.Time `gorm:"autoCreateTime"`
}

type UptimeLog struct {
	ID        uint      `gorm:"primaryKey"`
	BotID     uint      `gorm:"not null"`
	Timestamp time.Time `gorm:"autoCreateTime"`
	Result    bool      `gorm:"not null"`
}

var (
	db        *gorm.DB
	bot       *tele.Bot
	adminIDs  map[int64]bool
)

func main() {
	// Initialize database
	initDB()

	// Initialize Telegram bot
	initBot()

	// Start keep-alive scheduler
	go startScheduler()

	// Start HTTP server for Render
	startHTTPServer()
}

// Initialize PostgreSQL connection
func initDB() {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL environment variable is required")
	}

	var err error
	db, err = gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database:", err)
	}

	// Auto-migrate schemas
	db.AutoMigrate(&Admin{}, &Bot{}, &UptimeLog{})
	log.Println("✅ Database connected and migrated")

	// Load admin IDs from database
	loadAdmins()
}

// Load admins from database into memory
func loadAdmins() {
	adminIDs = make(map[int64]bool)
	
	var admins []Admin
	db.Find(&admins)
	
	for _, admin := range admins {
		adminIDs[admin.TelegramID] = true
	}

	// Also load from environment variable (for initial setup)
	envAdmins := os.Getenv("ADMIN_IDS")
	if envAdmins != "" {
		ids := strings.Split(envAdmins, ",")
		for _, idStr := range ids {
			id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
			if err == nil {
				adminIDs[id] = true
				// Add to database if not exists
				db.FirstOrCreate(&Admin{TelegramID: id})
			}
		}
	}

	log.Printf("✅ Loaded %d admin(s)", len(adminIDs))
}

// Initialize Telegram bot
func initBot() {
	botToken := os.Getenv("BOT_TOKEN")
	if botToken == "" {
		log.Fatal("BOT_TOKEN environment variable is required")
	}

	pref := tele.Settings{
		Token:  botToken,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	var err error
	bot, err = tele.NewBot(pref)
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}

	// Register command handlers
	bot.Handle("/start", handleStart)
	bot.Handle("/help", handleHelp)
	bot.Handle("/addbot", handleAddBot)
	bot.Handle("/removebot", handleRemoveBot)
	bot.Handle("/listbots", handleListBots)
	bot.Handle("/stats", handleStats)
	bot.Handle("/addadmin", handleAddAdmin)

	log.Println("✅ Telegram bot initialized")

	// Start bot in goroutine
	go bot.Start()
}

// Middleware: Check if user is admin
func isAdmin(userID int64) bool {
	return adminIDs[userID]
}

// Handler: /start
func handleStart(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized. This bot is for admins only.")
	}

	message := `👋 Welcome to Keep-Alive Bot!

I help monitor and keep your Telegram bots alive by sending periodic pings.

Use /help to see available commands.`

	return c.Send(message)
}

// Handler: /help
func handleHelp(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	helpText := `📖 Available Commands:

/addbot <username> <url> <interval> - Add bot to monitor
   Example: /addbot @mybot https://mybot.onrender.com 5

/removebot <username> - Remove bot from monitoring
   Example: /removebot @mybot

/listbots - Show all monitored bots

/stats - View uptime statistics

/addadmin <user_id> - Add new admin (super admin only)

/help - Show this help message`

	return c.Send(helpText)
}

// Handler: /addbot
func handleAddBot(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	args := strings.Fields(c.Text())
	if len(args) < 4 {
		return c.Send("❌ Usage: /addbot <username> <url> <interval_minutes>\nExample: /addbot @mybot https://mybot.onrender.com 5")
	}

	username := strings.TrimPrefix(args[1], "@")
	botURL := args[2]
	interval, err := strconv.Atoi(args[3])
	if err != nil || interval < 1 {
		return c.Send("❌ Invalid interval. Must be a positive number.")
	}

	// Validate URL
	if !strings.HasPrefix(botURL, "http://") && !strings.HasPrefix(botURL, "https://") {
		return c.Send("❌ Invalid URL. Must start with http:// or https://")
	}

	// Check if bot already exists
	var existingBot Bot
	result := db.Where("bot_username = ?", username).First(&existingBot)
	if result.RowsAffected > 0 {
		return c.Send("❌ Bot already exists in monitoring list.")
	}

	// Add new bot
	newBot := Bot{
		BotUsername:     username,
		BotURL:          botURL,
		IntervalMinutes: interval,
		Status:          "Unknown",
		AddedBy:         c.Sender().ID,
		LastPing:        time.Now(),
	}

	db.Create(&newBot)

	return c.Send(fmt.Sprintf("✅ Bot @%s added successfully!\nURL: %s\nPing interval: %d minutes", username, botURL, interval))
}

// Handler: /removebot
func handleRemoveBot(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	args := strings.Fields(c.Text())
	if len(args) < 2 {
		return c.Send("❌ Usage: /removebot <username>\nExample: /removebot @mybot")
	}

	username := strings.TrimPrefix(args[1], "@")

	result := db.Where("bot_username = ?", username).Delete(&Bot{})
	if result.RowsAffected == 0 {
		return c.Send("❌ Bot not found in monitoring list.")
	}

	return c.Send(fmt.Sprintf("✅ Bot @%s removed from monitoring.", username))
}

// Handler: /listbots
func handleListBots(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	var bots []Bot
	db.Find(&bots)

	if len(bots) == 0 {
		return c.Send("📭 No bots are currently being monitored.")
	}

	message := "🤖 Monitored Bots:\n\n"
	for i, b := range bots {
		statusEmoji := "❓"
		if b.Status == "Online" {
			statusEmoji = "✅"
		} else if b.Status == "Offline" {
			statusEmoji = "❌"
		}

		message += fmt.Sprintf("%d. @%s %s\n   URL: %s\n   Interval: %d min | Last Ping: %s\n\n",
			i+1, b.BotUsername, statusEmoji, b.BotURL, b.IntervalMinutes,
			b.LastPing.Format("02 Jan 15:04"))
	}

	return c.Send(message)
}

// Handler: /stats
func handleStats(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	var bots []Bot
	db.Find(&bots)

	if len(bots) == 0 {
		return c.Send("📭 No bots to show statistics for.")
	}

	message := "📊 Uptime Statistics:\n\n"

	for _, b := range bots {
		var totalLogs, successLogs int64
		db.Model(&UptimeLog{}).Where("bot_id = ?", b.ID).Count(&totalLogs)
		db.Model(&UptimeLog{}).Where("bot_id = ? AND result = ?", b.ID, true).Count(&successLogs)

		uptime := 0.0
		if totalLogs > 0 {
			uptime = (float64(successLogs) / float64(totalLogs)) * 100
		}

		message += fmt.Sprintf("@%s\n", b.BotUsername)
		message += fmt.Sprintf("  Status: %s\n", b.Status)
		message += fmt.Sprintf("  Uptime: %.2f%%\n", uptime)
		message += fmt.Sprintf("  Total Pings: %d\n\n", totalLogs)
	}

	return c.Send(message)
}

// Handler: /addadmin
func handleAddAdmin(c tele.Context) error {
	if !isAdmin(c.Sender().ID) {
		return c.Send("❌ Unauthorized.")
	}

	args := strings.Fields(c.Text())
	if len(args) < 2 {
		return c.Send("❌ Usage: /addadmin <telegram_user_id>")
	}

	newAdminID, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return c.Send("❌ Invalid user ID.")
	}

	// Add to database
	admin := Admin{TelegramID: newAdminID}
	result := db.FirstOrCreate(&admin, Admin{TelegramID: newAdminID})

	if result.RowsAffected == 0 {
		return c.Send("ℹ️ Admin already exists.")
	}

	// Add to memory
	adminIDs[newAdminID] = true

	return c.Send(fmt.Sprintf("✅ Admin %d added successfully!", newAdminID))
}

// Scheduler: Send keep-alive pings
func startScheduler() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	log.Println("✅ Scheduler started")

	for range ticker.C {
		var bots []Bot
		db.Find(&bots)

		for _, b := range bots {
			if time.Since(b.LastPing).Minutes() >= float64(b.IntervalMinutes) {
				go sendKeepAlivePing(b)
			}
		}
	}
}

// Send keep-alive ping to bot
func sendKeepAlivePing(b Bot) {
	log.Printf("📡 Pinging @%s at %s...", b.BotUsername, b.BotURL)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Try to ping the bot's URL
	resp, err := client.Get(b.BotURL)
	
	success := false
	status := "Offline"

	if err == nil {
		defer resp.Body.Close()
		// Consider 2xx and 3xx as success
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			success = true
			status = "Online"
			log.Printf("✅ Successfully pinged @%s (Status: %d)", b.BotUsername, resp.StatusCode)
		} else {
			log.Printf("⚠️ Bot @%s responded with status %d", b.BotUsername, resp.StatusCode)
		}
	} else {
		log.Printf("❌ Failed to ping @%s: %v", b.BotUsername, err)
	}

	// Update bot status and last ping time
	db.Model(&Bot{}).Where("id = ?", b.ID).Updates(map[string]interface{}{
		"last_ping": time.Now(),
		"status":    status,
	})

	// Log the ping result
	db.Create(&UptimeLog{
		BotID:  b.ID,
		Result: success,
	})

	// Alert admin if bot goes offline
	if !success {
		notifyAdminOffline(b)
	}
}

// Notify admin when bot goes offline
func notifyAdminOffline(b Bot) {
	var admin Admin
	db.Where("telegram_id = ?", b.AddedBy).First(&admin)
	
	if admin.TelegramID != 0 {
		recipient := &tele.User{ID: admin.TelegramID}
		message := fmt.Sprintf("⚠️ Alert: Bot @%s is OFFLINE!\n\nURL: %s\nLast successful ping: %s",
			b.BotUsername, b.BotURL, b.LastPing.Format("02 Jan 2006 15:04"))
		bot.Send(recipient, message)
	}
}

// HTTP Server for Render health checks
func startHTTPServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "🤖 Keep-Alive Bot is running!\nTime: %s", time.Now().Format(time.RFC3339))
	})

	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	log.Printf("🌐 HTTP server starting on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal("HTTP server failed:", err)
	}
}
