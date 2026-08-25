package main

// 报告输出（控制台）。与 report.go 分离：那里定义数据结构与编排，
// 这里只负责人读格式，改动排版不应触碰采集与判定逻辑。

import (
	"fmt"
	"os"
	"strings"
)

func (h *harness) printHeader(rep report) {
	fmt.Println(strings.Repeat("=", 78))
	fmt.Println("unirate 压测 —— 环境声明（绝对数字不可作生产参考）")
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("  宿主：%s\n", rep.Env.Host)
	for _, c := range rep.Env.Caveats {
		fmt.Printf("  · %s\n", c)
	}
	fmt.Printf("  客户端运行时：%s\n", rep.Meta.Client)
	fmt.Printf("  场景=%s 并发档=%v 轮数=%d 单轮=%v 预热=%v\n",
		rep.Meta.Scenario, rep.Meta.Concs, rep.Meta.Rounds,
		rep.Meta.Duration, rep.Meta.Warmup)
	if rep.Meta.Label != "" {
		fmt.Printf("  标签：%s\n", rep.Meta.Label)
	}
	fmt.Println(strings.Repeat("=", 78))
}

func (h *harness) printReport(rep report) {
	fmt.Println("\n" + strings.Repeat("=", 78))
	fmt.Println("中位数汇总（ADR-008：取中位数而非最优值）")
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("%-8s %-12s %-10s %-12s %-12s %-12s %s\n",
		"并发", "QPS中位数", "离散度", "p50", "p99", "TTFBp99", "Redis核数")
	for _, m := range rep.Median {
		fmt.Printf("%-8d %-12.0f %-10s %-12s %-12s %-12s %.3f\n",
			m.Conc, m.QPSMedian, fmtFloat(m.QPSSpread, 1)+"%",
			m.P50Median, m.P99Median, orElse(m.TTFBMedian != "", m.TTFBMedian, "-"),
			m.RedisCores)
	}

	fmt.Println("\n瓶颈归因：" + rep.Attrib)

	fmt.Println("\n" + strings.Repeat("-", 78))
	fmt.Println("判定（阈值按 ADR-008 §四.7 预先声明）")
	fmt.Println(strings.Repeat("-", 78))
	for _, c := range rep.Verdict.Checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s\n         %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("\n结论：%s\n", orElse(rep.Verdict.Pass, "PASS", "FAIL"))
	if !rep.Verdict.Pass {
		fmt.Fprintln(os.Stderr, "存在未通过项，退出码 2")
	}
}
