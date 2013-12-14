package main

import (
  "bytes"
  "config"
  "fmt"
  "io"
  _ "io/ioutil"
  "log"
  "net"
  "net/http"
  "net/url"
  "strings"
  "time"
)

const (
  CLR_N = "\x1b[0m"
  /* you use codes 30+i to specify foreground color, 40+i to specify background color */
  BLACK   = 0
  RED     = 1
  GREEN   = 2
  YELLO   = 3
  BLUE    = 4
  MAGENTA = 5
  CYAN    = 6
  WHITE   = 7
)

//返回ANSI 控制台颜色格式的字符串
//bc 背景颜色
//fc 前景(文字)颜色
func ansi_color(bc int, fc int, s string) string {
  return fmt.Sprintf("\x1b[%d;%dm%s%s", 40+bc, 30+fc, s, CLR_N)
}

/**
http Header Copay
*/
func header_copy(s http.Header, d *http.Header) {
  for hk, _ := range s {
    d.Set(hk, s.Get(hk))
  }
}

func access_log(w http.ResponseWriter, r *http.Request, query_url string, startTime time.Time) {
  remoteAddr := strings.Split(r.RemoteAddr, ":")[0] //客户端地址

  switch {
  case len(r.Header.Get("X-Real-Ip")) > 0:
    remoteAddr = r.Header.Get("X-Real-Ip")
  case len(r.Header.Get("Remote-Addr")) > 0:
    remoteAddr = r.Header.Get("Remote-Addr")
  case len(r.Header.Get("X-Forwarded-For")) > 0:
    remoteAddr = r.Header.Get("X-Forwarded-For")
  }

  if remoteAddr == "[" || len(remoteAddr) == 0 {
    remoteAddr = "127.0.0.1"
  }

  r.ParseForm()
  var postValues []string
  for k, _ := range r.PostForm {
    postValues = append(postValues, fmt.Sprintf("%s=%s", k, r.FormValue(k)))
  }

  if len(postValues) == 0 {
    postValues = append(postValues, "-")
  }

  content_len := w.Header().Get("Content-Length")
  if len(content_len) == 0 {
    content_len = "-"
  }

  logLine := fmt.Sprintf(`[%s] [%s] S:"%s %s %s F:{%s}" D:"%s F:{%s} %s %s",%0.5fs`,
    ansi_color(WHITE, BLACK, remoteAddr),
    time.Now().Format("2006-01-02 15:04:05.999999999 -0700 MST"),
    r.Method,
    ansi_color(GREEN, BLACK, r.RequestURI),
    r.Proto,
    ansi_color(GREEN, BLACK, strings.Join(postValues, "&")),
    ansi_color(CYAN, BLACK, query_url),
    ansi_color(CYAN, BLACK, strings.Join(postValues, "&")),
    w.Header().Get("Status"),
    content_len,
    time.Now().Sub(startTime).Seconds(),
  )
  g_env.AccessLog.Println(logLine)
}

func parse_querys(r *http.Request) (raw_query []string) {
  r.ParseForm()
  for k, _ := range r.Form {
    raw_query = append(raw_query, fmt.Sprintf("%s=%s", k, r.Form.Get(k)))
  }
  if len(r.Referer()) > 0 {
    if uri, err := url.Parse(r.Referer()); err == nil {
      for k, _ := range uri.Query() {
        raw_query = append(raw_query, fmt.Sprintf("%s=%s", k, uri.Query().Get(k)))
      }
    }
  }
  return
}

/**
获取目标地址
*/
func target_path(r *http.Request, cfg *config.Config) (t string) {
  if len(cfg.TargetPath) > 0 {
    t = cfg.TargetPath
  } else {
    t = r.URL.Path
  }
  return
}

/**
获取目标服务服务器
*/
func target_server(cfg *config.Config) (s string) {
  if len(cfg.TargetServer) > 0 {
    s = cfg.TargetServer
  } else {
    s = g_config.Default.TargetServer
  }
  return
}

/**
获取查询参数并做替换
*/
func swap_raw_query(r *http.Request, cfg *config.Config) (q string) {
  var tmpSlice []string
  if len(cfg.TargetParamNameSwap) == 0 {
    for k, _ := range r.URL.Query() {
      tmpSlice = append(tmpSlice, fmt.Sprintf("%s=%s", k, url.QueryEscape(r.URL.Query().Get(k))))
    }
    q = strings.Join(tmpSlice, "&")
    return
  }

  for k, _ := range r.URL.Query() {
    if v, ok := cfg.TargetParamNameSwap[k]; ok {
      tmpSlice = append(tmpSlice, fmt.Sprintf("%s=%s", v, url.QueryEscape(r.URL.Query().Get(k))))
    } else {
      tmpSlice = append(tmpSlice, fmt.Sprintf("%s=%s", k, url.QueryEscape(r.URL.Query().Get(k))))
    }
  }
  q = strings.Join(tmpSlice, "&")
  return
}

func timeout_dialer(conn_timeout int, rw_timeout int) func(net, addr string) (c net.Conn, err error) {
  return func(netw, addr string) (net.Conn, error) {
    conn, err := net.DialTimeout(netw, addr, time.Duration(conn_timeout)*time.Second)
    if err != nil {
      log.Printf("Failed to connect to [%s]. Timed out after %d seconds\n", addr, rw_timeout)
      return nil, err
    }
    conn.SetDeadline(time.Now().Add(time.Duration(rw_timeout) * time.Second))
    return conn, nil
  }
}

func handler_func(w http.ResponseWriter, r *http.Request) {
  var (
    cfg                                  *config.Config
    cfg_err                              *config.ConfigErr
    conntction_timeout, response_timeout int
    req                                  *http.Request
    err                                  error
    raw_query                            []string
  )
  defer func() {
    if re := recover(); re != nil {
      g_env.ErrorLog.Println("Recovered in backendServer:", re)
    }
  }()

  defer r.Body.Close()

  start_at := time.Now()
  raw_query = parse_querys(r)

  if err != nil {
    g_env.ErrorLog.Println(req, err)
    http.Error(w, "Read Body Error.", http.StatusInternalServerError)
    return
  }

  //获取配置文件
  if cfg, cfg_err = g_config.FindBySourcePathAndParams(raw_query, r.URL.Path); cfg_err != nil {
    cfg = g_config.FindByParamsOrSourcePath(raw_query, r.URL.Path)
  }

  if conntction_timeout = cfg.ConnectionTimeout; conntction_timeout <= 0 {
    conntction_timeout = 15
  }

  if response_timeout = cfg.ResponseTimeout; response_timeout <= 0 {
    response_timeout = 120
  }

  transport := http.Transport{
    Dial: timeout_dialer(conntction_timeout, response_timeout),
    ResponseHeaderTimeout: time.Duration(response_timeout) * time.Second,
    DisableCompression:    false,
    DisableKeepAlives:     true,
    MaxIdleConnsPerHost:   2,
  }
  defer transport.CloseIdleConnections()

  client := &http.Client{
    Transport: &transport,
  }

  query_url, _ := url.Parse(target_server(cfg) + target_path(r, cfg) + "?" + swap_raw_query(r, cfg))

  switch r.Method {
  case "GET", "HEAD":
    req, err = http.NewRequest(r.Method, query_url.String(), nil)
  case "POST":
    req, err = http.NewRequest(r.Method, query_url.String(), bytes.NewBufferString(strings.Join(raw_query, "&")))
    req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
  default:
    http.Error(w, "MethodNotAllowed", http.StatusMethodNotAllowed)
    return
  }
  req.Close = true

  header_copy(r.Header, &req.Header)

  if err != nil {
    g_env.ErrorLog.Println(err)
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
  }

  resp, err := client.Do(req)
  defer resp.Body.Close()

  if err != nil {
    g_env.ErrorLog.Println(req, err)
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
  }

  for hk, _ := range resp.Header {
    w.Header().Set(hk, resp.Header.Get(hk))
  }
  w.Header().Set("X-Transit-Ver", "0.0.1")

  if len(w.Header().Get("Server")) == 0 {
    w.Header().Set("Server", "X-Transit")
  }

  w.WriteHeader(resp.StatusCode)
  io.Copy(w, resp.Body)
  access_log(w, r, query_url.String(), start_at)
}

func Run() {
  g_env.ErrorLog.Printf("start@ %s:%d %v \n", g_config.Listen.Host, g_config.Listen.Port, time.Now())
  fmt.Printf("start@ %s:%d %v \n", g_config.Listen.Host, g_config.Listen.Port, time.Now())
  s := &http.Server{
    Addr:           fmt.Sprintf("%s:%d", g_config.Listen.Host, g_config.Listen.Port),
    Handler:        http.HandlerFunc(handler_func),
    ReadTimeout:    15 * time.Second,
    WriteTimeout:   15 * time.Second,
    MaxHeaderBytes: 1 << 20,
  }
  log.Fatal(s.ListenAndServe())
}
