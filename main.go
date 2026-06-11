package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

var (
	client      *whatsmeow.Client
	clientMu    sync.RWMutex
	webhookURL  string
	currentQR   string
	qrMu        sync.RWMutex
)

func main() {
	webhookURL = os.Getenv("WEBHOOK_URL")
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "/data/wa.db"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	os.MkdirAll("/data", 0755)

	dbLog := waLog.Stdout("DB", "WARN", true)
	container, err := sqlstore.New(context.Background(), "sqlite", fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", dbPath), dbLog)
	if err != nil {
		panic(err)
	}

	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		panic(err)
	}

	clientLog := waLog.Stdout("WA", "INFO", true)
	client = whatsmeow.NewClient(deviceStore, clientLog)
	client.AddEventHandler(eventHandler)

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(recover.New())
	app.Use(logger.New(logger.Config{Format: "${time} ${method} ${path} ${status}\n"}))

	app.Get("/health", handleHealth)
	app.Get("/status", handleStatus)
	app.Get("/qr", handleQR)
	app.Get("/qr/image", handleQRImage)
	app.Get("/qr-page", handleQRPage)
	app.Post("/connect", handleConnect)
	app.Post("/disconnect", handleDisconnect)
	app.Post("/send/text", handleSendText)
	app.Post("/send/image", handleSendImage)

	go connectClient()

	go func() {
		if err := app.Listen(":" + port); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	fmt.Printf("wa-service running on :%s\n", port)

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	clientMu.RLock()
	if client != nil && client.IsConnected() {
		client.Disconnect()
	}
	clientMu.RUnlock()
	app.Shutdown()
}

func connectClient() {
	clientMu.RLock()
	c := client
	clientMu.RUnlock()

	if c.Store.ID == nil {
		qrChan, _ := c.GetQRChannel(context.Background())
		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					qrMu.Lock()
					currentQR = evt.Code
					qrMu.Unlock()
					fmt.Println("New QR code available at /qr")
				}
			}
		}()
		c.Connect()
	} else {
		c.Connect()
	}
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		if webhookURL == "" {
			return
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"type":      "message",
			"from":      v.Info.Sender.String(),
			"chat":      v.Info.Chat.String(),
			"timestamp": v.Info.Timestamp.Unix(),
			"text":      v.Message.GetConversation(),
			"is_group":  v.Info.IsGroup,
		})
		go http.Post(webhookURL, "application/json", nil)
		_ = payload
	case *events.Connected:
		fmt.Println("WhatsApp connected")
		qrMu.Lock()
		currentQR = ""
		qrMu.Unlock()
	case *events.Disconnected:
		fmt.Println("WhatsApp disconnected")
	}
}

func handleHealth(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"ok": true})
}

func handleStatus(c *fiber.Ctx) error {
	clientMu.RLock()
	defer clientMu.RUnlock()
	connected := client != nil && client.IsConnected()
	loggedIn := client != nil && client.Store.ID != nil
	jid := ""
	if loggedIn {
		jid = client.Store.ID.String()
	}
	return c.JSON(fiber.Map{
		"connected": connected,
		"logged_in": loggedIn,
		"jid":       jid,
	})
}

func handleQR(c *fiber.Ctx) error {
	qrMu.RLock()
	qr := currentQR
	qrMu.RUnlock()
	if qr == "" {
		return c.Status(404).JSON(fiber.Map{"error": "no QR available — already connected or not initialized"})
	}
	return c.JSON(fiber.Map{"qr": qr})
}

func handleQRImage(c *fiber.Ctx) error {
	qrMu.RLock()
	qr := currentQR
	qrMu.RUnlock()
	if qr == "" {
		return c.Status(404).SendString("No QR available")
	}
	png, err := qrcode.Encode(qr, qrcode.Medium, 256)
	if err != nil {
		return c.Status(500).SendString(err.Error())
	}
	c.Set("Content-Type", "image/png")
	return c.Send(png)
}

func handleConnect(c *fiber.Ctx) error {
	clientMu.RLock()
	connected := client != nil && client.IsConnected()
	clientMu.RUnlock()
	if connected {
		return c.JSON(fiber.Map{"ok": true, "message": "already connected"})
	}
	go connectClient()
	return c.JSON(fiber.Map{"ok": true, "message": "connecting..."})
}

func handleDisconnect(c *fiber.Ctx) error {
	clientMu.RLock()
	c2 := client
	clientMu.RUnlock()
	if c2 != nil {
		c2.Disconnect()
	}
	return c.JSON(fiber.Map{"ok": true})
}

type SendTextReq struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

func handleSendText(c *fiber.Ctx) error {
	var req SendTextReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}
	if req.To == "" || req.Text == "" {
		return c.Status(400).JSON(fiber.Map{"error": "to and text required"})
	}

	clientMu.RLock()
	cl := client
	clientMu.RUnlock()
	if cl == nil || !cl.IsConnected() {
		return c.Status(503).JSON(fiber.Map{"error": "not connected"})
	}

	jid, err := types.ParseJID(req.To)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid JID: " + err.Error()})
	}

	msg := &waProto.Message{
		Conversation: proto.String(req.Text),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cl.SendMessage(ctx, jid, msg)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "id": resp.ID, "timestamp": resp.Timestamp})
}

type SendImageReq struct {
	To      string `json:"to"`
	URL     string `json:"url"`
	Caption string `json:"caption"`
}

func handleQRPage(c *fiber.Ctx) error {
	c.Set("Content-Type", "text/html; charset=utf-8")
	return c.SendString(`<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>WhatsApp QR</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0d1117;color:#e6edf3;font-family:-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:32px;text-align:center;max-width:320px;width:100%}
h1{font-size:18px;font-weight:600;margin-bottom:8px}
p{font-size:13px;color:#8b949e;margin-bottom:24px}
#qr-wrap{width:256px;height:256px;margin:0 auto 20px;border-radius:8px;overflow:hidden;background:#fff;display:flex;align-items:center;justify-content:center}
#qr-img{width:256px;height:256px}
#status{font-size:12px;color:#8b949e}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:#238636;margin-right:6px;vertical-align:middle}
.dot.orange{background:#d29922}
</style>
</head>
<body>
<div class="card">
  <h1>Conectar WhatsApp</h1>
  <p>Abre WhatsApp &gt; Dispositivos vinculados &gt; Vincular dispositivo</p>
  <div id="qr-wrap"><img id="qr-img" src="/wa/qr/image" alt="QR"/></div>
  <div id="status"><span class="dot orange"></span>Esperando escaneo...</div>
</div>
<script>
var img=document.getElementById('qr-img');
var st=document.getElementById('status');
var dot=st.querySelector('.dot');
function refresh(){
  var ts=new Date().getTime();
  img.src='/wa/qr/image?t='+ts;
}
function checkStatus(){
  fetch('/wa/status').then(r=>r.json()).then(function(d){
    if(d.connected&&d.logged_in){
      dot.classList.remove('orange');
      st.innerHTML='<span class="dot"></span>Conectado: '+d.jid.split('@')[0];
      clearInterval(qrTimer);
      clearInterval(stTimer);
    }
  }).catch(function(){});
}
var qrTimer=setInterval(refresh,20000);
var stTimer=setInterval(checkStatus,5000);
checkStatus();
</script>
</body>
</html>`)
}

func handleSendImage(c *fiber.Ctx) error {
	var req SendImageReq
	if err := c.BodyParser(&req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": err.Error()})
	}
	if req.To == "" || req.URL == "" {
		return c.Status(400).JSON(fiber.Map{"error": "to and url required"})
	}

	clientMu.RLock()
	cl := client
	clientMu.RUnlock()
	if cl == nil || !cl.IsConnected() {
		return c.Status(503).JSON(fiber.Map{"error": "not connected"})
	}

	jid, err := types.ParseJID(req.To)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid JID: " + err.Error()})
	}

	resp2, err := http.Get(req.URL)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "failed to fetch image: " + err.Error()})
	}
	defer resp2.Body.Close()
	imgData, err := io.ReadAll(resp2.Body)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "failed to read image: " + err.Error()})
	}

	uploaded, err := cl.Upload(context.Background(), imgData, whatsmeow.MediaImage)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "upload failed: " + err.Error()})
	}

	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			Caption:       proto.String(req.Caption),
			URL:           proto.String(uploaded.URL),
			DirectPath:    proto.String(uploaded.DirectPath),
			MediaKey:      uploaded.MediaKey,
			Mimetype:      proto.String("image/jpeg"),
			FileEncSHA256: uploaded.FileEncSHA256,
			FileSHA256:    uploaded.FileSHA256,
			FileLength:    proto.Uint64(uploaded.FileLength),
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sendResp, err := cl.SendMessage(ctx, jid, msg)
	if err != nil {
		return c.Status(500).JSON(fiber.Map{"error": err.Error()})
	}
	return c.JSON(fiber.Map{"ok": true, "id": sendResp.ID, "timestamp": sendResp.Timestamp})
}
