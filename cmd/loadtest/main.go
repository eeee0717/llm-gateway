// loadtest 测经过网关比直连上游多出多少首 token 延迟。
//
// 两端各打同样的流式请求，交替进行，报告两条延迟分布和它们的差。
//
//	loadtest -direct http://localhost:9090/v1 -gateway http://localhost:8080/v1 \
//	         -gateway-key sk-... -model mock-model -n 500 -c 20
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
	"syscall"
	"text/tabwriter"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
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
		gatewayKey = fs.String("gateway-key", "", "网关的 API Key")
		model      = fs.String("model", "", "模型名，两端一致")
		n          = fs.Int("n", 200, "采样对数，每对在两端各打一次")
		workers    = fs.Int("c", 10, "并发")
		interval   = fs.Duration("interval", 0, "两个请求之间的最小间隔，用来迁就上游的速率限制")
		extra      = fs.String("extra", "", `并进请求体的额外字段，JSON 对象，例如 '{"max_tokens":1}'`)
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *direct == "" || *model == "" {
		return errors.New("-direct 和 -model 是必填的")
	}

	body, err := requestBody(*model, *extra)
	if err != nil {
		return err
	}
	targets := [2]target{
		{Name: "direct", URL: *direct, Key: *directKey},
		{Name: "gateway", URL: *gateway, Key: *gatewayKey},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	started := time.Now()
	rep, err := measure(ctx, newClient(*workers), targets, body, *n, *workers, *interval)
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
			return nil, fmt.Errorf("-extra 不是合法的 JSON 对象: %w", err)
		}
	}
	body["model"] = model
	body["stream"] = true
	body["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
	return json.Marshal(body)
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
