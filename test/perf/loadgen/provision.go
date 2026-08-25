package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// perfBizB 是场景 B 专用的业务域。
//
// 为何不复用 demo：demo 上挂了 4 条规则（fixed 10/1s + sliding 30/10s
// + concurrency 50 + token 预算），500 并发打过去会先被 limit=10 的规则拦住，
// 通过数恒为 10 而非期望的 50，且无法判断是哪条规则生效 ——
// 「恰好通过 limit 个」这个断言在多规则叠加下没有单一归因。
//
// perfb 只挂一条 fixed_window 规则，维度为 biz（不含 ip/token，
// 避免压测客户端源端口变化或 token 分散导致落到不同 Key）。
const perfBizB = "perfb"

// perfBizThroughput 是吞吐类场景（A/C/D）专用业务域。
//
// 为何必须独立：demo 挂着 limit=10/1s 的规则，压测打过去
// 44 万请求里只有 210 个 200、其余全是 429 ——
// 那测的是**拒绝路径**，不是吞吐。实测踩过这个坑：
// 场景 A 首版直接打 demo，QPS 看着有 2.2 万，实际全是被拒的请求，
// 完全无法反映准入路径（TokenAdmit + Check + 转发上游）的真实成本。
//
// perft 的配额刻意设得极高（1e9），使限流恒不触发，
// 从而测到「规则求值 + Redis 往返 + 上游转发」的完整链路。
// 规则集则刻意与 demo 生产配置同构（fixed + sliding + conc + token 四条），
// 保证脚本成本（cjson 解码规模、Redis 命令数）与真实场景一致 ——
// 否则会重演「用简化脚本推导出 88k」的口径错误。
const perfBizThroughput = "perft"

// throughputRules 与 demo 同构但配额极高的规则集。
// 维度与窗口和 demo 一致，只把 limit 抬到不可能触发的量级。
func throughputRules() []map[string]any {
	const huge = 1000000000
	return []map[string]any{
		{"name": "perf-t-fixed", "type": "rate", "metric": "request",
			"dimensions": []string{"biz", "ip"}, "window": "1s",
			"limit": huge, "algorithm": "fixed_window", "enabled": true},
		{"name": "perf-t-sliding", "type": "rate", "metric": "request",
			"dimensions": []string{"biz", "token"}, "window": "10s",
			// sliding 用 ZSet 存成员，limit 受 Validate 限制（>100000 会被拒），
			// 故取上限 100000；配合每 worker 独立 token 分散 Key，不会触发
			"limit": 100000, "algorithm": "sliding_window", "enabled": true},
		{"name": "perf-t-conc", "type": "concurrency",
			"dimensions": []string{"biz"}, "max_concurrent": huge,
			"timeout": 120, "enabled": true},
		{"name": "perf-t-token", "type": "rate", "metric": "token",
			"dimensions": []string{"biz", "token"}, "window": "1h",
			"limit": huge, "algorithm": "fixed_window", "enabled": true},
	}
}

// ensureThroughputBiz 建立/更新吞吐场景专属业务域，并临时抬高全局兜底规则。
//
// 全局规则这一步是必须的，也是实测踩出来的：
// config.Store.Rules() 会把 biz="*" 的规则合并进**每一个** biz
// （见 internal/config/store.go 的 Rules 实现），
// 而 demo 数据里的 global-qps-guard 是 limit=5000/1s。
// 只抬高 perft 自己的配额没有用 —— 实测 200 占比仍只有 39%，
// 3,336,486 次拒绝全部来自 global-qps-guard。
// 5000 QPS 的天花板会让「吞吐压测」变成「测全局兜底规则」。
func (h *harness) ensureThroughputBiz() error {
	if h.cfg.AdminToken == "" {
		return fmt.Errorf("吞吐场景需要 ADMIN_TOKEN 以创建专属业务域 %q"+
			"（demo 的 limit=10/1s 会让请求几乎全被 429，测到的是拒绝路径而非吞吐）",
			perfBizThroughput)
	}
	if err := h.raiseGlobalGuard(); err != nil {
		return err
	}
	return h.upsertBiz(perfBizThroughput, throughputRules())
}

// globalGuardBackup 保存原始全局规则，压测结束后还原
var globalGuardBackup []map[string]any

// raiseGlobalGuard 把 biz="*" 的全局兜底规则临时抬到不触发的量级。
//
// 会先备份原值，由 restoreGlobalGuard 还原 ——
// 压测程序修改了共享配置就必须负责还原，
// 否则会把生产兜底规则永久改坏（这是压测工具最容易造成的次生伤害）。
func (h *harness) raiseGlobalGuard() error {
	cur, err := h.fetchBizRules("*")
	if err != nil {
		return fmt.Errorf("读取全局规则失败: %w", err)
	}
	if cur == nil {
		return nil // 无全局规则，无需处理
	}
	globalGuardBackup = cur

	raised := make([]map[string]any, 0, len(cur))
	for _, r := range cur {
		c := map[string]any{}
		for k, v := range r {
			c[k] = v
		}
		// 只抬高 rate 类的 limit，保持其余字段与维度不变
		if c["type"] == "rate" {
			c["limit"] = 1000000000
		}
		if c["type"] == "concurrency" {
			c["max_concurrent"] = 1000000000
		}
		raised = append(raised, c)
	}
	fmt.Println("[准备] 临时抬高全局兜底规则（结束后自动还原）")
	return h.upsertBizRaw("*", "", raised)
}

// restoreGlobalGuard 还原全局兜底规则
func (h *harness) restoreGlobalGuard() {
	if globalGuardBackup == nil {
		return
	}
	if err := h.upsertBizRaw("*", "", globalGuardBackup); err != nil {
		fmt.Printf("警告：全局兜底规则还原失败，请手动检查 biz=\"*\"：%v\n", err)
		return
	}
	fmt.Println("[清理] 全局兜底规则已还原")
	globalGuardBackup = nil
}

// fetchBizRules 读取指定 biz 的规则原始表示
func (h *harness) fetchBizRules(biz string) ([]map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, h.cfg.AdminURL+"/admin/bizs", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /admin/bizs 返回 %d", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Biz     string           `json:"biz"`
			BaseURL string           `json:"base_url"`
			Rules   []map[string]any `json:"rules"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	for _, it := range out.Items {
		if it.Biz == biz {
			return it.Rules, nil
		}
	}
	return nil, nil
}

// ensureScenarioBBiz 通过 admin API 建立/更新 perfb 业务域。
//
// 依赖 ADMIN_TOKEN。缺失时返回明确错误而非静默退化到 demo ——
// 静默退化会产出「通过 10 个」这种看似失败实则配置不对的结果，
// 属于典型的误导性数据。
func (h *harness) ensureScenarioBBiz(limit int64) error {
	if h.cfg.AdminToken == "" {
		return fmt.Errorf("场景 B 需要 ADMIN_TOKEN 以创建专属业务域 %q（只有单条规则才能对"+
			"「恰好通过 %d 个」做断言）；请设置 ADMIN_TOKEN 环境变量", perfBizB, limit)
	}

	return h.upsertBiz(perfBizB, []map[string]any{{
		"name":       "perf-b-fixed",
		"type":       "rate",
		"metric":     "request",
		"dimensions": []string{"biz"},
		"window":     "1s",
		"limit":      limit,
		"algorithm":  "fixed_window",
		"enabled":    true,
	}})
}

// upsertBiz 写入业务域并等待其在网关快照中生效
func (h *harness) upsertBiz(biz string, rules []map[string]any) error {
	return h.upsertBizRaw(biz, h.cfg.UpstreamBase, rules)
}

// upsertBizRaw 允许指定 base_url（全局 biz="*" 的 base_url 必须留空）
func (h *harness) upsertBizRaw(biz, baseURL string, rules []map[string]any) error {
	enabled := true
	payload := map[string]any{
		"biz":               biz,
		"base_url":          baseURL,
		"path_strip_prefix": true,
		"enabled":           enabled,
		"operator":          "perf-loadgen",
		"rules":             rules,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, h.cfg.AdminURL+"/admin/bizs", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("创建业务域 %s: %w", biz, err)
	}
	defer resp.Body.Close()
	// 202 = 已写入 SoT 但发布 Redis 失败，配置仍会由轮询兜底生效
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated &&
		resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("创建业务域 %s 返回 %d（期望 200/201/202）", biz, resp.StatusCode)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.bizVisible(biz) {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("业务域 %s 创建后 20s 内未在网关快照中生效", biz)
}

// bizVisible 检查网关当前快照是否已包含指定 biz
func (h *harness) bizVisible(biz string) bool {
	req, err := http.NewRequest(http.MethodGet, h.cfg.AdminURL+"/admin/snapshot", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+h.cfg.AdminToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var snap struct {
		Bizs map[string]json.RawMessage `json:"bizs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return false
	}
	_, ok := snap.Bizs[biz]
	return ok
}
