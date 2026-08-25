package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// 极简 RESP 客户端 —— 只实现压测所需的 AUTH / INFO / FLUSHDB / CONFIG RESETSTAT / DEL。
//
// 为何不用 go-redis：压测程序刻意保持**零第三方依赖**，
// 这样它在离线内网、无 GOPROXY 的环境下也能 `go run` 直接跑起来
// （本项目定位是基础设施组件，交付环境常常没有外网）。
// 代价是要自己处理 RESP 协议，但用到的命令极少、响应形态简单，风险可控。

type respConn struct {
	c  net.Conn
	br *bufio.Reader
}

func dialRedis(addr, password string, timeout time.Duration) (*respConn, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial redis %s: %w", addr, err)
	}
	rc := &respConn{c: c, br: bufio.NewReader(c)}
	if password != "" {
		if _, err := rc.do("AUTH", password); err != nil {
			rc.Close()
			return nil, fmt.Errorf("redis auth: %w", err)
		}
	}
	return rc, nil
}

func (r *respConn) Close() { _ = r.c.Close() }

// do 发送命令并读取一条回复。返回值对 bulk/simple string 均为其内容。
func (r *respConn) do(args ...string) (string, error) {
	_ = r.c.SetDeadline(time.Now().Add(10 * time.Second))

	var sb strings.Builder
	sb.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		sb.WriteString("$" + strconv.Itoa(len(a)) + "\r\n")
		sb.WriteString(a + "\r\n")
	}
	if _, err := r.c.Write([]byte(sb.String())); err != nil {
		return "", err
	}
	return r.readReply()
}

func (r *respConn) readReply() (string, error) {
	line, err := r.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 3 {
		return "", fmt.Errorf("malformed reply %q", line)
	}
	body := strings.TrimRight(line[1:], "\r\n")
	switch line[0] {
	case '+', ':':
		return body, nil
	case '-':
		return "", fmt.Errorf("redis error: %s", body)
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil {
			return "", err
		}
		if n < 0 {
			return "", nil // null bulk
		}
		buf := make([]byte, n+2) // 含尾部 CRLF
		if _, err := ioReadFull(r.br, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	case '*':
		// 压测只用到数组回复的存在性（如 KEYS），逐条丢弃即可
		n, err := strconv.Atoi(body)
		if err != nil {
			return "", err
		}
		var parts []string
		for i := 0; i < n; i++ {
			p, err := r.readReply()
			if err != nil {
				return "", err
			}
			parts = append(parts, p)
		}
		return strings.Join(parts, "\n"), nil
	default:
		return "", fmt.Errorf("unknown reply type %q", line[0])
	}
}

func ioReadFull(br *bufio.Reader, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := br.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// info 读取 INFO 的指定 section 并解析
func (r *respConn) info(section string) (redisStats, error) {
	var out redisStats
	all := ""
	for _, s := range []string{section, "commandstats"} {
		v, err := r.do("INFO", s)
		if err != nil {
			return out, err
		}
		all += v + "\n"
	}
	return parseRedisInfo(all), nil
}

// resetStat 清空 commandstats，使本轮统计不含预热与历史数据。
// 这是修正首版口径错误的关键操作 —— 累积统计会把不同脚本混在一起。
func (r *respConn) resetStat() error {
	_, err := r.do("CONFIG", "RESETSTAT")
	return err
}

// flushDB 清空限流计数器。
//
// 安全说明：配置快照存放在 Redis（unirate:config:snapshot），
// FLUSHDB 会连带清掉它。网关本地有 atomic.Pointer 缓存故不会立即失效，
// 但为稳妥起见，压测流程在 FLUSHDB 后调用 admin /admin/reload
// 让网关从 MySQL(SoT) 重新加载并重新发布快照。
func (r *respConn) flushDB() error {
	_, err := r.do("FLUSHDB")
	return err
}

func (r *respConn) delKeys(keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	_, err := r.do(append([]string{"DEL"}, keys...)...)
	return err
}

func (r *respConn) ping() error {
	v, err := r.do("PING")
	if err != nil {
		return err
	}
	if !strings.EqualFold(v, "PONG") {
		return fmt.Errorf("unexpected ping reply %q", v)
	}
	return nil
}
