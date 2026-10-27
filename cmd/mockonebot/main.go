// Command mockonebot 是本地联调用的假协议端 + 假模型服务。
//
// 用途：开发机不装 Docker、不连真 QQ，也能把「消息进来 → 判断 → 组上下文 →
// 调模型 → 分段发出」整条链路跑起来，并且能看见每一步的结果。
//
// 起三个服务：
//
//	127.0.0.1:3001  OneBot v11 WebSocket 服务端（NapCat 的位置）
//	127.0.0.1:3010  OpenAI 兼容模型服务（DeepSeek 的位置）
//	127.0.0.1:3020  控制接口 + 操作面板（浏览器打开即可点按钮造消息）
//
// 典型用法：
//
//	go run ./cmd/mockonebot -db /tmp/qqbot-dev/data/qqbot/qqbot.db
//	QQBOT_ROOT=/tmp/qqbot-dev go run ./cmd/qqbot run
//	浏览器打开 http://127.0.0.1:3020
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/vex/qqbot/internal/mock"
	"github.com/vex/qqbot/internal/store"
)

func main() {
	var (
		onebotAddr = flag.String("onebot", "127.0.0.1:3001", "mock 协议端监听地址")
		llmAddr    = flag.String("llm", "127.0.0.1:3010", "mock 模型服务监听地址")
		ctlAddr    = flag.String("ctl", "127.0.0.1:3020", "控制接口监听地址")
		token      = flag.String("token", "", "协议端 access token（需与 etc/secrets.env 一致）")
		selfID     = flag.Int64("self", 10001, "机器人 QQ 号")
		nick       = flag.String("nick", "小助手", "机器人昵称")
		reply      = flag.String("reply", "", "固定回复；留空则按「收到：<用户消息>」生成")
		dbPath     = flag.String("db", "", "可选：把该 qqbot.db 的模型配置指向本 mock")
	)
	flag.Parse()

	ob, err := mock.NewOneBot(mock.OneBotOptions{
		Addr:     *onebotAddr,
		SelfID:   *selfID,
		Nickname: *nick,
		Token:    *token,
		Groups: []mock.Group{
			{GroupID: 30001, GroupName: "测试群", MemberCount: 3},
			{GroupID: 30002, GroupName: "摸鱼群", MemberCount: 8},
		},
		Friends: []mock.Friend{
			{UserID: 20001, Nickname: "小明"},
			{UserID: 20002, Nickname: "小红"},
		},
	})
	if err != nil {
		fail("启动 mock 协议端失败: %v", err)
	}
	defer ob.Close()

	ls, err := mock.NewLLM(mock.LLMOptions{Addr: *llmAddr, Reply: *reply})
	if err != nil {
		fail("启动 mock 模型服务失败: %v", err)
	}
	defer ls.Close()

	if *dbPath != "" {
		if err := seedDB(*dbPath, ls.URL()); err != nil {
			fail("写入模型配置失败: %v", err)
		}
		fmt.Printf("✓ 已把 %s 的模型配置指向 %s（API Key 用占位值 sk-mock）\n", *dbPath, ls.URL())
	}

	ctl := &controller{ob: ob, llm: ls, selfID: *selfID}
	ctlSrv := &http.Server{
		Addr:              *ctlAddr,
		Handler:           ctl.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := ctlSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fail("控制接口启动失败: %v", err)
		}
	}()

	printBanner(ob.URL(), ls.URL(), *ctlAddr, *selfID)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go watchConnections(ctx, ob)

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = ctlSrv.Shutdown(shutdownCtx)
	fmt.Println("\n已退出")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", args...)
	os.Exit(1)
}

func printBanner(onebotURL, llmURL, ctlAddr string, selfID int64) {
	fmt.Printf(`
================ qqbot 本地联调环境 ================
  协议端 (WebSocket)  %s
  模型服务 (BaseURL)  %s
  控制面板            http://%s
  机器人 QQ 号        %d

下一步：
  1) 让 qqbot 指向本 mock：
       QQBOT_ROOT=<你的根目录> ./bin/qqbot run
     若 qqbot.toml 里 onebot.url 不是上面这个地址，改掉再启动。
  2) 打开控制面板，点「发一条群消息」，看右侧是否出现回复。
  3) 也可以用 curl：
       curl -s -XPOST http://%s/inject -d '{"group_id":30001,"user_id":20001,"text":"你好","at_self":true}'

Ctrl+C 退出。
====================================================

`, onebotURL, llmURL, ctlAddr, selfID, ctlAddr)
}

// watchConnections 在连接状态变化时打一行提示，方便判断 qqbot 有没有连上来。
func watchConnections(ctx context.Context, ob *mock.OneBotServer) {
	// 从 0 开始，避免启动时先打一条假的"已断开"
	last := 0
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := ob.Connected()
			if n == last {
				continue
			}
			last = n
			if n > 0 {
				fmt.Printf("[%s] qqbot 已连接（当前 %d 个连接）\n", time.Now().Format("15:04:05"), n)
			} else {
				fmt.Printf("[%s] 连接已断开\n", time.Now().Format("15:04:05"))
			}
		}
	}
}

// seedDB 把目标数据库的模型配置指向 mock，省得手工改面板。
func seedDB(path, llmURL string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()

	_, err = st.UpdateSettings(func(v *store.Settings) error {
		v.LLM.BaseURL = llmURL
		v.LLM.APIKey = "sk-mock"
		v.LLM.Model = "deepseek-flash"
		return nil
	})
	return err
}

type controller struct {
	ob     *mock.OneBotServer
	llm    *mock.LLMServer
	selfID int64
}

func (c *controller) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", c.handleIndex)
	mux.HandleFunc("/state", c.handleState)
	mux.HandleFunc("/sent", c.handleSent)
	mux.HandleFunc("/inject", c.handleInject)
	mux.HandleFunc("/reply", c.handleReply)
	mux.HandleFunc("/fail", c.handleFail)
	mux.HandleFunc("/drop", c.handleDrop)
	mux.HandleFunc("/reset", c.handleReset)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (c *controller) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"connected":  c.ob.Connected(),
		"conn_total": c.ob.ConnTotal(),
		"sent":       len(c.ob.Sent()),
		"llm_calls":  c.llm.Calls(),
		"self_id":    c.selfID,
	})
}

func (c *controller) handleSent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.ob.Sent())
}

func (c *controller) handleInject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
		return
	}
	var body struct {
		GroupID   int64  `json:"group_id"`
		UserID    int64  `json:"user_id"`
		Nickname  string `json:"nickname"`
		Text      string `json:"text"`
		AtSelf    bool   `json:"at_self"`
		AtAll     bool   `json:"at_all"`
		Image     bool   `json:"image"`
		Private   bool   `json:"private"`
		MessageID int64  `json:"message_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体解析失败: " + err.Error()})
		return
	}
	if body.UserID == 0 {
		body.UserID = 20001
	}
	if body.GroupID == 0 && !body.Private {
		body.GroupID = 30001
	}

	id, err := c.ob.InjectMessage(mock.InjectMessage{
		GroupID: body.GroupID, UserID: body.UserID, Nickname: body.Nickname,
		Text: body.Text, AtSelf: body.AtSelf, AtAll: body.AtAll,
		Image: body.Image, Private: body.Private, MessageID: body.MessageID,
	})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message_id": id})
}

func (c *controller) handleReply(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	c.llm.SetReply(body.Text)
	writeJSON(w, http.StatusOK, map[string]string{"reply": body.Text})
}

func (c *controller) handleFail(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 1
	}
	c.llm.FailNext(n)
	writeJSON(w, http.StatusOK, map[string]int{"fail_next": n})
}

func (c *controller) handleDrop(w http.ResponseWriter, r *http.Request) {
	c.ob.DropPeers()
	writeJSON(w, http.StatusOK, map[string]string{"status": "dropped"})
}

func (c *controller) handleReset(w http.ResponseWriter, r *http.Request) {
	c.ob.Reset()
	c.llm.Reset()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}

func (c *controller) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

// indexHTML 是一个自包含的操作面板。
// 刻意写得极简：它只是开发工具，不引入构建链。
const indexHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>qqbot mock 控制台</title>
<style>
  :root{--bg:#0f1115;--card:#171a21;--line:#262b36;--fg:#e6e9ef;--dim:#8b93a7;--ok:#3fb950;--bad:#f85149;--accent:#4c8dff}
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.6 -apple-system,"Segoe UI",system-ui,sans-serif}
  .wrap{max-width:960px;margin:0 auto;padding:24px}
  h1{font-size:18px;margin:0 0 16px;font-weight:600}
  .card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px}
  .row{display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end}
  label{display:block;font-size:12px;color:var(--dim);margin-bottom:4px}
  input[type=text],input[type=number]{background:#0c0e13;border:1px solid var(--line);color:var(--fg);border-radius:6px;padding:7px 9px;font:inherit;width:100%}
  .f{flex:1 1 120px}
  .f.wide{flex:1 1 320px}
  button{background:var(--accent);border:0;color:#fff;border-radius:6px;padding:8px 14px;font:inherit;cursor:pointer}
  button.ghost{background:#22262f;color:var(--fg)}
  button:hover{filter:brightness(1.1)}
  .stat{display:flex;gap:20px;flex-wrap:wrap}
  .stat div{font-size:12px;color:var(--dim)}
  .stat b{display:block;font-size:20px;color:var(--fg);font-weight:600}
  .dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:6px}
  .list{max-height:380px;overflow:auto;font-family:ui-monospace,Consolas,monospace;font-size:12px}
  .item{border-bottom:1px solid var(--line);padding:8px 0}
  .item:last-child{border-bottom:0}
  .tag{display:inline-block;background:#22262f;border-radius:4px;padding:1px 6px;color:var(--dim);margin-right:6px}
  .chk{display:flex;align-items:center;gap:6px;color:var(--fg);font-size:13px;padding-bottom:8px}
  .chk input{width:auto}
</style></head><body><div class="wrap">
<h1>qqbot mock 控制台</h1>

<div class="card stat">
  <div><span class="dot" id="dot" style="background:var(--bad)"></span>协议端连接<b id="conn">0</b></div>
  <div>已发消息<b id="sent">0</b></div>
  <div>模型调用<b id="calls">0</b></div>
  <div>累计连接<b id="total">0</b></div>
</div>

<div class="card">
  <div class="row" style="margin-bottom:10px">
    <div class="f"><label>群号</label><input type="number" id="gid" value="30001"></div>
    <div class="f"><label>QQ 号</label><input type="number" id="uid" value="20001"></div>
    <div class="f wide"><label>消息内容</label><input type="text" id="text" value="你好，帮我看看今天该干嘛"></div>
  </div>
  <div class="row" style="margin-bottom:10px">
    <div class="chk"><input type="checkbox" id="at" checked><label for="at" style="margin:0">@ 机器人</label></div>
    <div class="chk"><input type="checkbox" id="img"><label for="img" style="margin:0">带图片</label></div>
    <div class="chk"><input type="checkbox" id="priv"><label for="priv" style="margin:0">私聊</label></div>
  </div>
  <div class="row">
    <button onclick="inject()">发一条消息</button>
    <button class="ghost" onclick="call('/drop')">断开连接（测重连）</button>
    <button class="ghost" onclick="call('/fail?n=3')">让模型失败 3 次</button>
    <button class="ghost" onclick="call('/reset')">清空记录</button>
  </div>
</div>

<div class="card">
  <div class="row" style="margin-bottom:10px">
    <div class="f wide"><label>固定回复内容（留空则按「收到：xxx」生成）</label><input type="text" id="replyText" placeholder="例如：好的，我知道了。"></div>
    <button onclick="setReply()">设置</button>
  </div>
  <div class="list" id="out"></div>
</div>

<script>
async function j(u,o){const r=await fetch(u,o);return r.json()}
async function inject(){
  const b={group_id:+gid.value,user_id:+uid.value,text:text.value,at_self:at.checked,image:img.checked,private:priv.checked};
  const r=await j('/inject',{method:'POST',body:JSON.stringify(b)});
  if(r.error){alert(r.error)}
}
async function setReply(){await j('/reply',{method:'POST',body:JSON.stringify({text:replyText.value})});alert('已设置')}
async function call(u){const r=await j(u);console.log(u,r)}
function esc(s){return String(s).replace(/[&<>]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))}
async function tick(){
  try{
    const s=await j('/state');
    conn.textContent=s.connected; sent.textContent=s.sent; calls.textContent=s.llm_calls; total.textContent=s.conn_total;
    dot.style.background = s.connected>0 ? 'var(--ok)' : 'var(--bad)';
    const list=await j('/sent');
    out.innerHTML = list.length? list.slice().reverse().map(m=>
      '<div class="item"><span class="tag">#'+m.seq+'</span><span class="tag">'+
      (m.action==='send_group_msg'?'群 '+m.group_id:'私聊 '+m.user_id)+'</span>'+
      (m.reply_to?'<span class="tag">引用 '+m.reply_to+'</span>':'')+
      (m.at_user?'<span class="tag">@'+m.at_user+'</span>':'')+
      esc(m.text)+'</div>').join('') : '<div style="color:var(--dim)">还没有发出任何消息</div>';
  }catch(e){}
}
setInterval(tick,800);tick();
</script></div></body></html>
`
