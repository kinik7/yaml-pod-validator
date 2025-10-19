package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// -------- инфраструктура ошибок --------

type vErr struct {
	line *int
	msg  string
}

type vCtx struct {
	filename string
	errs     []vErr
}

func (v *vCtx) addErr(line *int, msg string) { v.errs = append(v.errs, vErr{line, msg}) }
func (v *vCtx) hasErrs() bool                { return len(v.errs) > 0 }

// flush:
// - required-поля (line == nil) печатаются без имени файла и строки: "<field> is required"
// - остальные сообщения: "<file>:<line> <text>"
func (v *vCtx) flush() {
	for _, e := range v.errs {
		if e.line != nil {
			fmt.Fprintf(os.Stdout, "%s:%d %s\n", v.filename, *e.line, e.msg)
		} else {
			fmt.Fprintf(os.Stdout, "%s\n", e.msg)
		}
	}
}

// -------- удобный просмотр YAML-мапы --------

type mapView struct {
	fields map[string]*yaml.Node // key -> value node
	lines  map[string]int        // key -> keyNode.Line
}

func viewMap(n *yaml.Node) (*mapView, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, errors.New("expected mapping node")
	}
	mv := &mapView{
		fields: map[string]*yaml.Node{},
		lines:  map[string]int{},
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		mv.fields[k.Value] = v
		mv.lines[k.Value] = k.Line
	}
	return mv, nil
}

func getMapping(n *yaml.Node) (*mapView, bool) {
	if n != nil && n.Kind == yaml.MappingNode {
		mv, err := viewMap(n)
		if err != nil {
			return nil, false
		}
		return mv, true
	}
	return nil, false
}

func getSequence(n *yaml.Node) ([]*yaml.Node, bool) {
	if n != nil && n.Kind == yaml.SequenceNode {
		return n.Content, true
	}
	return nil, false
}

func getScalarString(n *yaml.Node) (string, bool) {
	if n != nil && n.Kind == yaml.ScalarNode {
		return n.Value, true
	}
	return "", false
}

// -------- правила/регэкспы --------

var (
	reSnakeCase = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)
	// registry.bigbrother.io/<path>:<tag>
	reImage = regexp.MustCompile(`^registry\.bigbrother\.io\/[a-z0-9._\/-]+:[A-Za-z0-9._-]+$`)
	// memory: 128Mi, 1Gi, 512Ki
	reMem = regexp.MustCompile(`^\d+(Gi|Mi|Ki)$`)
)

func portInRange(p int) bool { return p > 0 && p < 65536 }

// -------- валидации --------

func validateTop(v *vCtx, root *yaml.Node) {
	// ожидаем DocumentNode с одним корневым объектом
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		v.addErr(nil, "document is required")
		return
	}
	obj := root.Content[0]
	mv, ok := getMapping(obj)
	if !ok {
		line := obj.Line
		v.addErr(&line, "document must be mapping")
		return
	}

	// apiVersion (required, "v1")
	api, ok := mv.fields["apiVersion"]
	if !ok {
		v.addErr(nil, "apiVersion is required")
	} else if s, ok := getScalarString(api); !ok {
		line := api.Line
		v.addErr(&line, "apiVersion must be string")
	} else if s != "v1" {
		line := api.Line
		v.addErr(&line, fmt.Sprintf("apiVersion has unsupported value '%s'", s))
	}

	// kind (required, "Pod")
	kind, ok := mv.fields["kind"]
	if !ok {
		v.addErr(nil, "kind is required")
	} else if s, ok := getScalarString(kind); !ok {
		line := kind.Line
		v.addErr(&line, "kind must be string")
	} else if s != "Pod" {
		line := kind.Line
		v.addErr(&line, fmt.Sprintf("kind has unsupported value '%s'", s))
	}

	// metadata (required)
	if meta, ok := mv.fields["metadata"]; ok {
		validateObjectMeta(v, meta)
	} else {
		v.addErr(nil, "metadata is required")
	}

	// spec (required)
	if spec, ok := mv.fields["spec"]; ok {
		validatePodSpec(v, spec)
	} else {
		v.addErr(nil, "spec is required")
	}
}

func validateObjectMeta(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "metadata must be object")
		return
	}
	// name (required)
	if name, ok := mv.fields["name"]; !ok {
		v.addErr(nil, "metadata.name is required")
	} else if _, ok := getScalarString(name); !ok {
		line := name.Line
		v.addErr(&line, "metadata.name must be string")
	}
	// namespace (optional)
	if ns, ok := mv.fields["namespace"]; ok {
		if _, ok := getScalarString(ns); !ok {
			line := ns.Line
			v.addErr(&line, "metadata.namespace must be string")
		}
	}
	// labels (optional: map[string]string)
	if labels, ok := mv.fields["labels"]; ok {
		lmv, ok := getMapping(labels)
		if !ok {
			line := labels.Line
			v.addErr(&line, "metadata.labels must be object")
		} else {
			for k, val := range lmv.fields {
				if _, ok := getScalarString(val); !ok {
					line := val.Line
					v.addErr(&line, fmt.Sprintf("metadata.labels.%s must be string", k))
				}
			}
		}
	}
}

func validatePodSpec(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "spec must be object")
		return
	}

	// os (optional): допускаем scalar или object{name: ...}
	if osNode, ok := mv.fields["os"]; ok {
		switch osNode.Kind {
		case yaml.ScalarNode:
			val := strings.ToLower(osNode.Value)
			if val != "linux" && val != "windows" {
				line := osNode.Line
				v.addErr(&line, fmt.Sprintf("os has unsupported value '%s'", osNode.Value))
			}
		case yaml.MappingNode:
			omv, _ := getMapping(osNode)
			nameNode, ok := omv.fields["name"]
			if !ok {
				v.addErr(nil, "name is required")
			} else if s, ok := getScalarString(nameNode); !ok {
				line := nameNode.Line
				v.addErr(&line, "name must be string")
			} else {
				val := strings.ToLower(s)
				if val != "linux" && val != "windows" {
					line := nameNode.Line
					v.addErr(&line, fmt.Sprintf("name has unsupported value '%s'", s))
				}
			}
		default:
			line := osNode.Line
			v.addErr(&line, "os must be string or object")
		}
	}

	// containers (required, non-empty array)
	cn, ok := mv.fields["containers"]
	if !ok {
		v.addErr(nil, "spec.containers is required")
		return
	}
	seq, ok := getSequence(cn)
	if !ok {
		line := cn.Line
		v.addErr(&line, "spec.containers must be array")
		return
	}
	if len(seq) == 0 {
		line := cn.Line
		v.addErr(&line, "spec.containers must not be empty")
	}
	for _, c := range seq {
		validateContainer(v, c)
	}
}

func validateContainer(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "containers[] must be object")
		return
	}

	// name (required, snake_case)
	if name, ok := mv.fields["name"]; !ok {
		v.addErr(nil, "containers.name is required")
	} else if s, ok := getScalarString(name); !ok {
		line := name.Line
		v.addErr(&line, "containers.name must be string")
	} else if !reSnakeCase.MatchString(s) {
		line := name.Line
		v.addErr(&line, fmt.Sprintf("containers.name has invalid format '%s'", s))
	}

	// image (required, registry.bigbrother.io + tag)
	if img, ok := mv.fields["image"]; !ok {
		v.addErr(nil, "containers.image is required")
	} else if s, ok := getScalarString(img); !ok {
		line := img.Line
		v.addErr(&line, "containers.image must be string")
	} else if !reImage.MatchString(s) {
		line := img.Line
		v.addErr(&line, fmt.Sprintf("containers.image has invalid format '%s'", s))
	}

	// ports (optional) — array of ContainerPort
	if ports, ok := mv.fields["ports"]; ok {
		if seq, ok := getSequence(ports); ok {
			for _, p := range seq {
				validateContainerPort(v, p)
			}
		} else {
			line := ports.Line
			v.addErr(&line, "ports must be array")
		}
	}

	// probes (optional)
	if rp, ok := mv.fields["readinessProbe"]; ok {
		validateProbe(v, rp)
	}
	if lp, ok := mv.fields["livenessProbe"]; ok {
		validateProbe(v, lp)
	}

	// resources (required)
	if res, ok := mv.fields["resources"]; ok {
		validateResources(v, res)
	} else {
		v.addErr(nil, "containers.resources is required")
	}
}

func validateContainerPort(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "ports must be object")
		return
	}

	// containerPort (required, int in (0,65536))
	cp, ok := mv.fields["containerPort"]
	if !ok {
		v.addErr(nil, "containerPort is required")
		return
	}
	s, ok := getScalarString(cp)
	if !ok {
		line := cp.Line
		v.addErr(&line, "containerPort must be int")
		return
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		line := cp.Line
		v.addErr(&line, "containerPort must be int")
	} else if !portInRange(val) {
		line := cp.Line
		// важно совпасть с автотестом:
		v.addErr(&line, "containerPort value out of range")
	}

	// protocol (optional, TCP|UDP)
	if proto, ok := mv.fields["protocol"]; ok {
		if s, ok := getScalarString(proto); !ok {
			line := proto.Line
			v.addErr(&line, "protocol must be string")
		} else if s != "TCP" && s != "UDP" {
			line := proto.Line
			v.addErr(&line, fmt.Sprintf("protocol has unsupported value '%s'", s))
		}
	}
}

func validateProbe(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "probe must be object")
		return
	}
	hg, ok := mv.fields["httpGet"]
	if !ok {
		v.addErr(nil, "httpGet is required")
		return
	}
	validateHTTPGet(v, hg)
}

func validateHTTPGet(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "httpGet must be object")
		return
	}

	// path (required, absolute)
	if path, ok := mv.fields["path"]; !ok {
		v.addErr(nil, "path is required")
	} else if s, ok := getScalarString(path); !ok {
		line := path.Line
		v.addErr(&line, "path must be string")
	} else if !strings.HasPrefix(s, "/") {
		line := path.Line
		v.addErr(&line, fmt.Sprintf("path has invalid format '%s'", s))
	}

	// port (required, int in (0,65536))
	if port, ok := mv.fields["port"]; !ok {
		v.addErr(nil, "port is required")
	} else if s, ok := getScalarString(port); !ok {
		line := port.Line
		v.addErr(&line, "port must be int")
	} else {
		val, err := strconv.Atoi(s)
		if err != nil {
			line := port.Line
			v.addErr(&line, "port must be int")
		} else if !portInRange(val) {
			line := port.Line
			v.addErr(&line, "port value out of range")
		}
	}
}

func validateResources(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "resources must be object")
		return
	}
	if req, ok := mv.fields["requests"]; ok {
		validateResourceSet(v, req)
	}
	if lim, ok := mv.fields["limits"]; ok {
		validateResourceSet(v, lim)
	}
}

func validateResourceSet(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "resources set must be object")
		return
	}
	for key, val := range mv.fields {
		switch key {
		case "cpu":
			// строго YAML int: !!int, а не строка
			if val == nil || val.Kind != yaml.ScalarNode || val.Tag != "!!int" {
				line := val.Line
				v.addErr(&line, "cpu must be int")
				continue
			}
			if _, err := strconv.Atoi(val.Value); err != nil {
				line := val.Line
				v.addErr(&line, "cpu must be int")
			}
		case "memory":
			if s, ok := getScalarString(val); !ok {
				line := val.Line
				v.addErr(&line, "memory must be string")
			} else if !reMem.MatchString(s) {
				line := val.Line
				v.addErr(&line, fmt.Sprintf("memory has invalid format '%s'", s))
			}
		default:
			// неизвестные ресурсы — игнорируем (строже не требовалось)
		}
	}
}

// -------- main --------

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <path-to-yaml>\n", os.Args[0])
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	arg := flag.Arg(0)
	// Автотест ожидает только базовое имя файла в сообщениях
	filename := filepath.Base(arg)

	data, err := os.ReadFile(arg)
	if err != nil {
		// ошибки чтения/десериализации пусть остаются в stderr
		fmt.Fprintf(os.Stderr, "%s: cannot read file content: %v\n", arg, err)
		os.Exit(1)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot unmarshal file content: %v\n", arg, err)
		os.Exit(1)
	}

	v := &vCtx{filename: filename}
	validateTop(v, &root)

	if v.hasErrs() {
		v.flush() // → stdout
		os.Exit(1)
	}
	os.Exit(0)
}
