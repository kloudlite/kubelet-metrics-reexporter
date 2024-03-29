package parser

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"text/template"

	"github.com/kloudlite/kubelet-metrics-reexporter/pkg/k8s"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

var (
	nodeCpuUsageMetricName       = []byte("node_cpu_usage_seconds_total")
	nodeMemUsageMetricName       = []byte("node_memory_working_set_bytes")
	containerCpuUsageMetricName  = []byte("container_cpu_usage_seconds_total")
	containerMemUsageMetricName  = []byte("container_memory_working_set_bytes")
	containerStartTimeMetricName = []byte("container_start_time_seconds")

	podCpuUsageMetricName  = []byte("pod_cpu_usage_seconds_total")
	podMemUsageMetricName  = []byte("pod_memory_working_set_bytes")
	podStartTimeMetricName = []byte("pod_start_time_seconds")

	kubeletVolumeStatsUsedBytes = []byte("kubelet_volume_stats_used_bytes")
)

var (
	namespaceTag     = []byte("namespace")
	podNameTag       = []byte("pod")
	containerNameTag = []byte("container")
)

type ParserOpts struct {
	PodsMap                   map[types.NamespacedName]corev1.Pod
	EnrichTags                map[string]string
	EnrichFromLabels          bool
	EnrichFromAnnotations     bool
	FilterPrefixes            []string
	ReplacePrefixes           map[string]string
	ShouldValidateMetricLabel bool
	ValidLabelRegexExpr       string

	labelValidator *regexp.Regexp
}

type Parser struct {
	kCli     *k8s.Client
	nodeName string
	ParserOpts
}

func NewParser(ctx context.Context, kcli *k8s.Client, nodeName string, opts ParserOpts) (*Parser, error) {
	r, err := regexp.Compile(opts.ValidLabelRegexExpr)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to compile metric label regexp")
	}

	opts.labelValidator = r

	pl, err := kcli.ListPodsOnNode(ctx, nodeName)
	if err != nil {
		return nil, err
	}

	opts.PodsMap = k8s.ToPodsMap(pl)

	return &Parser{
		kCli:       kcli,
		nodeName:   nodeName,
		ParserOpts: opts,
	}, nil
}

func (p *Parser) validateTagName(key string) bool {
	if p.ShouldValidateMetricLabel && !p.labelValidator.MatchString(key) {
		return false
	}
	return true
}

func (p *Parser) filterTagName(key string) bool {
	if len(p.FilterPrefixes) == 0 {
		return true
	}

	for i := range p.FilterPrefixes {
		if strings.HasPrefix(key, p.FilterPrefixes[i]) {
			return true
		}
	}
	return false
}

func (p *Parser) getSanitizedTagName(key string) string {
	for k, v := range p.ReplacePrefixes {
		if strings.HasPrefix(key, k) {
			return v + key[len(k):]
		}
	}
	return key
}

func (p *Parser) ParseAndEnhanceMetricsInto(b []byte, writer io.Writer) error {
	b = append(b, []byte("\n")...)
	reader := bufio.NewReader(bytes.NewBuffer(b))

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				writer.Write(line)
				break
			}
		}

		if line[0] == '#' {
			if _, err := writer.Write(line); err != nil {
				return err
			}
			continue
		}

		tagStart := bytes.Index(line, []byte("{"))
		tagEnd := bytes.Index(line, []byte("}"))

		if tagStart == -1 || tagEnd == -1 || tagStart >= tagEnd {
			// INFO: when input, does not correspond to this format `{....}`, skip operating on it
			if _, err := writer.Write(line); err != nil {
				return err
			}
			continue
		}

		tagBytes := line[tagStart+1 : tagEnd]

		namespace, podName, containerName := parseContainerLabels(tagBytes)

		nn := types.NamespacedName{Namespace: namespace, Name: podName}

		tags := make([]string, 0, len(p.PodsMap[nn].Labels)+3+len(p.EnrichTags))

		if containerName != "" {
			tags = append(tags, fmt.Sprintf("%s=%q", containerNameTag, containerName))
		}
		tags = append(tags, fmt.Sprintf("%s=%q", namespaceTag, namespace))
		tags = append(tags, fmt.Sprintf("%s=%q", podNameTag, podName))

		if p.EnrichFromLabels {
			for k, v := range p.PodsMap[nn].Labels {
				if p.filterTagName(k) {
					nk := p.getSanitizedTagName(k)
					if p.validateTagName(nk) {
						tags = append(tags, fmt.Sprintf("%s=%q", nk, v))
					}
				}
			}
		}

		if p.EnrichFromAnnotations {
			for k, v := range p.PodsMap[nn].Annotations {
				if p.filterTagName(k) {
					nk := p.getSanitizedTagName(k)
					if p.validateTagName(nk) {
						tags = append(tags, fmt.Sprintf("%s=%q", nk, v))
					}
				}
			}
		}

		for k, v := range p.EnrichTags {
			if p.validateTagName(k) {
				t := template.New("sample")
				if _, err := t.Parse(v); err != nil {
					return err
				}
				buff := new(bytes.Buffer)
				if err := t.Execute(buff, p.PodsMap[nn]); err != nil {
					return err
				}

				tags = append(tags, fmt.Sprintf("%s=%q", k, buff.String()))
			}
		}

		x := fmt.Sprintf("{%s}", strings.Join(tags, ","))
		out := string(line[:tagStart]) + x + string(line[tagEnd+1:])
		if _, err := writer.Write([]byte(out)); err != nil {
			return err
		}
	}

	return nil
}

func parseContainerLabels(tags []byte) (namespace, podName, containerName string) {
	b := bytes.Split(tags, []byte(","))
	for i := range b {
		b2 := bytes.Split(b[i], []byte("="))

		if bytes.Equal(b2[0], containerNameTag) {
			containerName = string(b2[1][1 : len(b2[1])-1])
			continue
		}

		if bytes.Equal(b2[0], namespaceTag) {
			namespace = string(b2[1][1 : len(b2[1])-1])
			continue
		}

		if bytes.Equal(b2[0], podNameTag) {
			podName = string(b2[1][1 : len(b2[1])-1])
		}
	}

	return
}
