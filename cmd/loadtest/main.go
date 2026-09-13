// loadtest 比较直连上游和经过网关这两端，两端打的是同样的流式请求。
//
// 两个模式量的是两件事：
//
//	latency     每端交替打 -n 对请求，比第一个 SSE 事件到达的时间
//	throughput  每端各打满 -duration 的时长，比每秒完成多少个完整请求
//
//	loadtest -direct http://localhost:9090/v1 -gateway http://localhost:8080/v1 \
//	         -gateway-key sk-... -model mock-model -n 500 -c 20
//
//	loadtest -mode throughput -direct ... -gateway ... -gateway-key k1,k2,k3 \
//	         -model mock-model -c 50 -duration 20s
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return // flag 包已经把用法打出来了
		}
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	var (
		direct     = fs.String("direct", "", "直连上游的地址，到 /v1 为止")
		directKey  = fs.String("direct-key", "", "直连上游用的密钥")
		gateway    = fs.String("gateway", "http://localhost:8080/v1", "网关的地址，到 /v1 为止")
		gatewayKey = fs.String("gateway-key", "", "网关的 API Key；用逗号分隔多个，请求轮流用，避开同一行上的预扣排队")
		model      = fs.String("model", "", "模型名，两端一致")
		n          = fs.Int("n", 200, "采样对数，每对在两端各打一次")
		workers    = fs.Int("c", 10, "并发")
		interval   = fs.Duration("interval", 0, "两个请求之间的最小间隔，用来迁就上游的速率限制")
		extra      = fs.String("extra", "", `并进请求体的额外字段，JSON 对象，例如 '{"max_tokens":1}'`)
		mode       = fs.String("mode", "latency", "latency 比首 token 延迟，throughput 比打满并发后的吞吐")
		duration   = fs.Duration("duration", 30*time.Second, "throughput 模式下每端各打多久")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *direct == "" || *model == "" {
		return errors.New("-direct and -model are required")
	}
	if *n < 1 || *workers < 1 {
		return errors.New("-n and -c must be greater than 0")
	}
	// 两个模式各认各的参数。默默忽略一个传进来的参数很危险：以为 -interval
	// 限住了速率才敢去打真实上游，而吞吐模式根本不看它。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	switch *mode {
	case "latency":
		if given["duration"] {
			return errors.New("-duration only applies to -mode throughput; latency mode runs -n pairs")
		}
	case "throughput":
		if given["interval"] {
			return errors.New("-mode throughput ignores -interval by design, it saturates; use latency mode to rate-limit")
		}
		if given["n"] {
			return errors.New("-mode throughput runs for -duration, not a fixed -n")
		}
	default:
		return fmt.Errorf("-mode must be latency or throughput, got %q", *mode)
	}

	body, err := requestBody(*model, *extra)
	if err != nil {
		return err
	}
	targets := [2]target{
		{Name: "direct", URL: *direct, Keys: keys(*directKey)},
		{Name: "gateway", URL: *gateway, Keys: keys(*gatewayKey)},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newClient(*workers)
	if *mode == "throughput" {
		return throughput(ctx, out, client, targets, body, *workers, *duration)
	}

	started := time.Now()
	rep, err := measure(ctx, client, targets, body, *n, *workers, *interval)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "model=%s pairs=%d workers=%d elapsed=%s\n\n",
		*model, *n, *workers, time.Since(started).Round(time.Millisecond))
	return write(out, rep)
}

// requestBody 拼出两端共用的请求体。extra 先展开，核心字段后写，
// 所以 -extra 只能往上加字段，改不动压测赖以成立的那几个。
func requestBody(model, extra string) ([]byte, error) {
	body := map[string]any{}
	if extra != "" {
		if err := json.Unmarshal([]byte(extra), &body); err != nil {
			return nil, fmt.Errorf("-extra is not a valid JSON object: %w", err)
		}
	}
	body["model"] = model
	body["stream"] = true
	body["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
	return json.Marshal(body)
}

// throughput 两端各打满一段时间，报每秒完成多少个完整请求。
func throughput(ctx context.Context, out io.Writer, c *http.Client, targets [2]target, body []byte, workers int, d time.Duration) error {
	var samples [2][]time.Duration
	var qps [2]float64
	for i, t := range targets {
		started := time.Now()
		got, err := saturate(ctx, c, t, body, workers, d)
		if err != nil {
			return err
		}
		if len(got) == 0 {
			return fmt.Errorf("%s: no request finished, raise -duration", t.Name)
		}
		samples[i], qps[i] = got, float64(len(got))/time.Since(started).Seconds()
	}

	fmt.Fprintf(out, "workers=%d duration=%s 每端分别打满；p50/p99 是整个请求读完的耗时，不是首 token 延迟\n\n", workers, d)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "\tdone\tQPS\tp50\tp99\t")
	for i, t := range targets {
		s := summarize(samples[i])
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%s\t%s\t\n", t.Name, s.N, qps[i], ms(s.P50), ms(s.P99))
	}
	fmt.Fprintf(w, "%s 的吞吐是 %s 的\t\t%.0f%%\t\t\t\n",
		targets[1].Name, targets[0].Name, 100*qps[1]/qps[0])
	return w.Flush()
}

// keys 把逗号分隔的密钥拆开，顺带丢掉空的。
func keys(s string) []string {
	var out []string
	for k := range strings.SplitSeq(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// newClient 的连接池要装得下所有 worker：默认每个主机只留 2 条空闲连接，
// 并发一高就每次重新建连，测出来的成了握手时间。不设 Timeout，它会截断流。
func newClient(workers int) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = workers
	t.MaxIdleConns = 2 * workers
	return &http.Client{Transport: t}
}

// write 打出两条分布和它们的差。差是负数也照实写：上游本身的抖动
// 常常盖过网关那点开销，把负号藏起来只会让人误以为测准了。
func write(out io.Writer, rep report) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "\tN\tmin\tp50\tp90\tp99\tmax\t")
	var s [2]summary
	for i, t := range rep.Targets {
		s[i] = summarize(rep.Samples[i])
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t\n", t.Name, s[i].N,
			ms(s[i].Min), ms(s[i].P50), ms(s[i].P90), ms(s[i].P99), ms(s[i].Max))
	}
	fmt.Fprintf(w, "%s 多出\t\t%s\t%s\t%s\t%s\t%s\t\n", rep.Targets[1].Name,
		diff(s[1].Min, s[0].Min), diff(s[1].P50, s[0].P50), diff(s[1].P90, s[0].P90),
		diff(s[1].P99, s[0].P99), diff(s[1].Max, s[0].Max))
	return w.Flush()
}

func ms(d time.Duration) string { return d.Round(100 * time.Microsecond).String() }

func diff(a, b time.Duration) string {
	if a >= b {
		return "+" + ms(a-b)
	}
	return "-" + ms(b-a)
}
