package login

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	qrcodeTerminal "github.com/Baozisoftware/qrcode-terminal-go"
	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/websocket"
	"github.com/tuotoo/qrcode"
	"io"
	"jtrans/jbox"
	"jtrans/login/models"
	"jtrans/tbox"
	"jtrans/utils"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	cli *http.Client
)

const (
	SessionPath = "./session.json"
	QRCodePath  = "./qrcode.png"
)

type Method = int

const (
	JAAuthCookie Method = iota
	Qrcode
)

func init() {
	jar, _ := cookiejar.New(nil)
	cli = &http.Client{Jar: jar}
}

func Login(method Method) error {
	session, _ := getPersistentSession()
	jaAuthCookie := ""
	if session != nil && len(session.JAAuthCookie) > 0 {
		jaAuthCookie = session.JAAuthCookie
		fmt.Println("读取到存储的 JAAuthCookie，尝试自动登录...")
	}

	if len(jaAuthCookie) > 0 || method == JAAuthCookie {
		return autoLogin(jaAuthCookie)
	}
	if method == Qrcode {
		return qrcodeLogin()
	}
	return nil
}

func GetClient() (jbox.IClient, tbox.IClient, error) {
	file, err := os.Open(SessionPath)
	if file == nil || err != nil {
		return nil, nil, err
	}
	decoder := json.NewDecoder(file)
	session := models.Session{}
	err = decoder.Decode(&session)
	if err != nil {
		return nil, nil, err
	}

	jcli, err := jbox.NewClient(session.JboxCookies)
	if err != nil {
		return nil, nil, err
	}

	tcli, err := tbox.NewClient(session.UserToken)
	if err != nil {
		return nil, nil, err
	}

	return jcli, tcli, nil
}

func getPersistentSession() (*models.Session, error) {
	var content []byte
	file, err := os.Open(SessionPath)
	if file != nil {
		content, err = io.ReadAll(file)
		if err != nil {
			return nil, err
		}
	}
	defer file.Close()

	session := &models.Session{}
	err = json.Unmarshal(content, session)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func getJAAuthCookie() (string, error) {
	fmt.Println("请确保登录之后前往 https://jaccount.sjtu.edu.cn/jaccount/ 在开发者工具中查看 cookie 并复制 JAAuthCookie 的值：")
	var jaAuthCookie string
	for {
		reader := bufio.NewReader(os.Stdin)
		input, _, err := reader.ReadLine()
		jaAuthCookie = string(input)
		if err != nil {
			return "", err
		}
		jaAuthCookie = strings.TrimSpace(jaAuthCookie)
		if len(jaAuthCookie) > 0 {
			break
		}
		fmt.Println("JAAuthCookie 不得为空！")
	}
	return jaAuthCookie, nil
}

func checkUserInfo() error {
	resp, err := cli.Get("https://my.sjtu.edu.cn/api/resource/my/info")
	if err != nil {
		return err
	}
	if !utils.IsSuccessStatusCode(resp.StatusCode) {
		return fmt.Errorf("服务器响应%d", resp.StatusCode)
	}

	user := models.UserInfo{}
	err = utils.UnmarshalJson[models.UserInfo](resp, &user)

	if user.Errno != 0 {
		return fmt.Errorf("服务器返回错误：%s", user.Error)
	}

	return nil
}

func validate(jaAuthCookie string) (*models.Session, error) {
	var (
		err         error
		jboxCookies string
		userToken   string
	)
	//err = checkUserInfo()
	//if err != nil {
	//	return nil, fmt.Errorf("验证失败：%s", err.Error())
	//}
	jboxCookies, err = loginJbox(jaAuthCookie)
	if err != nil {
		return nil, fmt.Errorf("jbox认证失败：%s", err.Error())
	}
	userToken, err = loginTbox(jaAuthCookie)
	if err != nil {
		return nil, fmt.Errorf("tbox认证失败：%s", err.Error())
	}

	return &models.Session{
		JboxCookies: jboxCookies,
		UserToken:   userToken,
	}, nil
}

func autoLogin(jaAuthCookie string) error {
	var (
		err     error
		file    *os.File
		session *models.Session
	)

	for {
		if jaAuthCookie == "" {
			jaAuthCookie, err = getJAAuthCookie()
			if err != nil {
				return err
			}
		}
		fmt.Printf("正在验证登录...")
		session, err = validate(jaAuthCookie)
		if err == nil {
			session.JAAuthCookie = jaAuthCookie
			break
		}
		jaAuthCookie = ""
		fmt.Println("登录失败！")
		fmt.Printf("原因：%s\n", err.Error())
	}
	fmt.Println("登录成功！")
	fmt.Printf("正在保存登录信息...")
	file, err = os.OpenFile(SessionPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	err = encoder.Encode(session)

	if err != nil {
		return err
	}
	fmt.Println("成功！")

	return nil
}

func getUuid() (string, error) {
	resp, err := cli.Get("https://my.sjtu.edu.cn/ui/appmyinfo")
	if err != nil {
		return "", err
	}
	redirect := strings.Contains(resp.Request.URL.String(), "https://jaccount.sjtu.edu.cn/jaccount/jalogin")
	if resp.StatusCode == http.StatusOK && !redirect {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("服务器返回状态%d", resp.StatusCode)
	}

	defer resp.Body.Close()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}
	node := doc.Find("input[type=hidden][name=uuid]")
	if node == nil {
		return "", fmt.Errorf("没有找到 uuid")
	}
	uuid, exists := node.Attr("value")
	if !exists {
		return "", fmt.Errorf("没有找到 uuid")
	}
	return uuid, nil
}

func initWebsocket(uuid string) (*websocket.Conn, error) {
	uri, _ := url.Parse(fmt.Sprintf("wss://jaccount.sjtu.edu.cn/jaccount/sub/%s", uuid))
	c, _, err := websocket.DefaultDialer.Dial(uri.String(), nil)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func getQRCodeURL(uuid, sig string, ts int64) string {
	return fmt.Sprintf("https://jaccount.sjtu.edu.cn/jaccount/qrcode?uuid=%s&ts=%d&sig=%s", uuid, ts, sig)
}

func downloadQRCodeURL(qrcodeURL string) error {
	file, err := os.OpenFile(QRCodePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	resp, err := cli.Get(qrcodeURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回状态：%d", resp.StatusCode)
	}

	_, err = io.Copy(file, resp.Body)
	return err
}

func showQRCode(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	qrmatrix, err := qrcode.Decode(file)
	if err != nil {
		return err
	}
	obj := qrcodeTerminal.New()
	obj.Get(qrmatrix.Content).Print()
	return nil
}

func sendUpdateQRCodeMessage(ws *websocket.Conn) {
	message := "{ \"type\": \"UPDATE_QR_CODE\" }"
	if err := ws.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
		_ = ws.Close()
	}
}

func sendUpdateQRCodeMessageWorker(ws *websocket.Conn, ctx context.Context) {
	sendUpdateQRCodeMessage(ws)
	ticker := time.Tick(time.Second * 50)
	for {
		select {
		case <-ticker:
			sendUpdateQRCodeMessage(ws)
		case <-ctx.Done():
			return
		}
	}
}

func handleScanSuccess(uuid string) error {
	resp, err := cli.Get(fmt.Sprintf("https://jaccount.sjtu.edu.cn/jaccount/expresslogin?uuid=%s", uuid))
	if err != nil {
		return err
	}

	if !utils.IsSuccessStatusCode(resp.StatusCode) {
		return fmt.Errorf("expresslogin失败，服务器返回%d", resp.StatusCode)
	}

	redirect := strings.Contains(resp.Request.URL.String(), "https://jaccount.sjtu.edu.cn/jaccount/jalogin")
	if resp.StatusCode == http.StatusOK && redirect {
		return fmt.Errorf("expresslogin失败，未认证")
	}

	return nil
}

func qrcodeLogin() error {
	var (
		err       error
		uuid      string
		ws        *websocket.Conn
		message   []byte
		payload   models.LoginPayload
		tp        string
		messageTp int
	)
	fmt.Println("正在使用二维码登录")
	uuid, err = getUuid()
	if err != nil {
		return err
	}
	ws, err = initWebsocket(uuid)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()
	go sendUpdateQRCodeMessageWorker(ws, ctx)
	for {
		messageTp, message, err = ws.ReadMessage()
		if messageTp != websocket.TextMessage {
			continue
		}
		err = json.Unmarshal(message, &payload)
		if err != nil {
			return fmt.Errorf("消息格式错误：%s", err.Error())
		}
		if payload.Error != 0 {
			return fmt.Errorf("登录错误：%d", payload.Error)
		}
		tp = strings.ToUpper(payload.Type)
		if tp == "UPDATE_QR_CODE" {
			qrcodeURL := getQRCodeURL(uuid, payload.Payload.Sig, payload.Payload.Ts)
			fmt.Printf("正在下载二维码...")
			if err = downloadQRCodeURL(qrcodeURL); err != nil {
				return err
			}
			fmt.Println("完毕")
			if err = showQRCode(QRCodePath); err != nil {
				return err
			}
			fmt.Println("请扫码登录！")
		} else if tp == "LOGIN" {
			if err = handleScanSuccess(uuid); err != nil {
				return err
			}
			fmt.Println("扫码成功！")
			break
		}
	}

	cookies := cli.Jar.Cookies(authUrl)
	for _, c := range cookies {
		if c.Name == "JAAuthCookie" {
			fmt.Println("读取到 JAAuthCookie，开始自动登录")
			return autoLogin(c.Value)
		}
	}
	return fmt.Errorf("未读取到 JAAuthCookie！")
}
