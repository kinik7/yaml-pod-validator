package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type vErr struct {
	line *int   // если есть строка - печатаем "<file>:<line> …", если нет — "<file>: …" (для required)
	msg  string // готовый текст ошибки по требованиям
}

type vCtx struct {
	filename string
	errs     []vErr
}

func (v *vCtx) addErr(line *int, msg string) {
	v.errs = append(v.errs, vErr{line: line, msg: msg})
}

func (v *vCtx) hasErrs() bool { return len(v.errs) > 0 }

func (v *vCtx) flush() {
	for _, e := range v.errs {
		if e.line != nil {
			fmt.Fprintf(os.Stderr, "%s:%d %s\n", v.filename, *e.line, e.msg)
		} else {
			// для отсутствующих обязательных полей — без номера строки
			fmt.Fprintf(os.Stderr, "%s: %s\n", v.filename, e.msg)
		}
	}
}

type mapView struct {
	fields map[string]*yaml.Node // key -> value node
	lines  map[string]int        // key -> keyNode.Line (строка, где ключ объявлен)
}

func viewMap(n *yaml.Node) (*mapView, error) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, errors.New("internal: expected mapping node")
	}
	mv := &mapView{
		fields: make(map[string]*yaml.Node),
		lines:  make(map[string]int),
	}
	// пары [key, value] идут подряд
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		v := n.Content[i+1]
		// ключи в манифестах — строки
		mv.fields[k.Value] = v
		mv.lines[k.Value] = k.Line
	}
	return mv, nil
}

func getScalarString(n *yaml.Node) (string, bool) {
	if n != nil && n.Kind == yaml.ScalarNode {
		return n.Value, true
	}
	return "", false
}

func getSequence(n *yaml.Node) ([]*yaml.Node, bool) {
	if n != nil && n.Kind == yaml.SequenceNode {
		return n.Content, true
	}
	return nil, false
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

// ----- Регэкспы и константы правил -----

var (
	reSnakeCase = regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*$`)
	// registry.bigbrother.io/<path>:<tag>
	reImage = regexp.MustCompile(`^registry\.bigbrother\.io\/[a-z0-9._\/-]+:[A-Za-z0-9._-]+$`)
	// memory: 128Mi, 1Gi, 512Ki
	reMem = regexp.MustCompile(`^\d+(Gi|Mi|Ki)$`)
)

func portInRange(p int) bool { return p > 0 && p < 65536 }

// ----- Валидация верхнего уровня -----

func validateTop(v *vCtx, root *yaml.Node) {
	// Документный корень: root.Kind == DocumentNode, вложенный MappingNode
	if root == nil || root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		v.addErr(nil, "document is required") // на всякий случай
		return
	}
	obj := root.Content[0]
	mv, ok := getMapping(obj)
	if !ok {
		// Не мапа — неправильный корень
		line := obj.Line
		v.addErr(&line, "document must be mapping")
		return
	}

	// 1) apiVersion (required, string == v1)
	apiNode, ok := mv.fields["apiVersion"]
	if !ok {
		v.addErr(nil, "apiVersion is required")
	} else {
		if s, ok := getScalarString(apiNode); !ok {
			line := apiNode.Line
			v.addErr(&line, "apiVersion must be string")
		} else if s != "v1" {
			line := apiNode.Line
			v.addErr(&line, fmt.Sprintf("apiVersion has unsupported value '%s'", s))
		}
	}

	// 2) kind (required, string == Pod)
	kindNode, ok := mv.fields["kind"]
	if !ok {
		v.addErr(nil, "kind is required")
	} else {
		if s, ok := getScalarString(kindNode); !ok {
			line := kindNode.Line
			v.addErr(&line, "kind must be string")
		} else if s != "Pod" {
			line := kindNode.Line
			v.addErr(&line, fmt.Sprintf("kind has unsupported value '%s'", s))
		}
	}

	// 3) metadata (required, ObjectMeta)
	metaNode, ok := mv.fields["metadata"]
	if !ok {
		v.addErr(nil, "metadata is required")
	} else {
		validateObjectMeta(v, metaNode)
	}

	// 4) spec (required, PodSpec)
	specNode, ok := mv.fields["spec"]
	if !ok {
		v.addErr(nil, "spec is required")
	} else {
		validatePodSpec(v, specNode)
	}
}

// ----- ObjectMeta -----

func validateObjectMeta(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "metadata must be object")
		return
	}
	// name (required, string)
	if nameNode, ok := mv.fields["name"]; !ok {
		v.addErr(nil, "metadata.name is required")
	} else {
		if _, ok := getScalarString(nameNode); !ok {
			line := nameNode.Line
			v.addErr(&line, "metadata.name must be string")
		}
	}
	// namespace (optional, string)
	if nsNode, ok := mv.fields["namespace"]; ok {
		if _, ok := getScalarString(nsNode); !ok {
			line := nsNode.Line
			v.addErr(&line, "metadata.namespace must be string")
		}
	}
	// labels (optional, object of string:string)
	if labelsNode, ok := mv.fields["labels"]; ok {
		lmv, ok := getMapping(labelsNode)
		if !ok {
			line := labelsNode.Line
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

// ----- PodSpec -----

func validatePodSpec(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "spec must be object")
		return
	}

	// os (optional): допускаем два варианта:
	//   а) scalar: "linux"|"windows"
	//   б) object: { name: "linux"|"windows" }
	if osNode, ok := mv.fields["os"]; ok {
		switch osNode.Kind {
		case yaml.ScalarNode:
			val := strings.ToLower(osNode.Value)
			if val != "linux" && val != "windows" {
				line := osNode.Line
				v.addErr(&line, fmt.Sprintf("spec.os has unsupported value '%s'", osNode.Value))
			}
		case yaml.MappingNode:
			omv, _ := getMapping(osNode)
			nameNode, ok := omv.fields["name"]
			if !ok {
				v.addErr(nil, "spec.os.name is required")
			} else if s, ok := getScalarString(nameNode); !ok {
				line := nameNode.Line
				v.addErr(&line, "spec.os.name must be string")
			} else {
				val := strings.ToLower(s)
				if val != "linux" && val != "windows" {
					line := nameNode.Line
					v.addErr(&line, fmt.Sprintf("spec.os.name has unsupported value '%s'", s))
				}
			}
		default:
			line := osNode.Line
			v.addErr(&line, "spec.os must be string or object")
		}
	}

	// containers (required) — sequence of Container
	contNode, ok := mv.fields["containers"]
	if !ok {
		v.addErr(nil, "spec.containers is required")
		return
	}
	seq, ok := getSequence(contNode)
	if !ok {
		line := contNode.Line
		v.addErr(&line, "spec.containers must be array")
		return
	}
	if len(seq) == 0 {
		line := contNode.Line
		v.addErr(&line, "spec.containers must not be empty")
	}
	for _, c := range seq {
		validateContainer(v, c)
	}
}

// ----- Container -----

func validateContainer(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "containers[] must be object")
		return
	}

	// name (required, snake_case)
	if nameNode, ok := mv.fields["name"]; !ok {
		v.addErr(nil, "containers.name is required")
	} else if s, ok := getScalarString(nameNode); !ok {
		line := nameNode.Line
		v.addErr(&line, "containers.name must be string")
	} else if !reSnakeCase.MatchString(s) {
		line := nameNode.Line
		v.addErr(&line, fmt.Sprintf("containers.name has invalid format '%s'", s))
	}

	// image (required, domain registry.bigbrother.io + tag)
	if imgNode, ok := mv.fields["image"]; !ok {
		v.addErr(nil, "containers.image is required")
	} else if s, ok := getScalarString(imgNode); !ok {
		line := imgNode.Line
		v.addErr(&line, "containers.image must be string")
	} else if !reImage.MatchString(s) {
		line := imgNode.Line
		v.addErr(&line, fmt.Sprintf("containers.image has invalid format '%s'", s))
	}

	// ports (optional) — array of ContainerPort
	if portsNode, ok := mv.fields["ports"]; ok {
		seq, ok := getSequence(portsNode)
		if !ok {
			line := portsNode.Line
			v.addErr(&line, "containers.ports must be array")
		} else {
			for _, p := range seq {
				validateContainerPort(v, p)
			}
		}
	}

	// readinessProbe (optional) — Probe
	if rpNode, ok := mv.fields["readinessProbe"]; ok {
		validateProbe(v, rpNode, "containers.readinessProbe")
	}
	// livenessProbe (optional) — Probe
	if lpNode, ok := mv.fields["livenessProbe"]; ok {
		validateProbe(v, lpNode, "containers.livenessProbe")
	}

	// resources (required) — ResourceRequirements
	if resNode, ok := mv.fields["resources"]; !ok {
		v.addErr(nil, "containers.resources is required")
	} else {
		validateResources(v, resNode)
	}
}

// ----- ContainerPort -----

func validateContainerPort(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "containers.ports[] must be object")
		return
	}
	// containerPort (required, int 1..65535)
	cpNode, ok := mv.fields["containerPort"]
	if !ok {
		v.addErr(nil, "containers.ports.containerPort is required")
	} else if s, ok := getScalarString(cpNode); !ok {
		line := cpNode.Line
		v.addErr(&line, "containers.ports.containerPort must be int")
	} else {
		val, err := strconv.Atoi(s)
		if err != nil {
			line := cpNode.Line
			v.addErr(&line, "containers.ports.containerPort must be int")
		} else if !portInRange(val) {
			line := cpNode.Line
			v.addErr(&line, "containers.ports.containerPort value out of range")
		}
	}

	// protocol (optional, TCP|UDP)
	if protoNode, ok := mv.fields["protocol"]; ok {
		if s, ok := getScalarString(protoNode); !ok {
			line := protoNode.Line
			v.addErr(&line, "containers.ports.protocol must be string")
		} else if s != "TCP" && s != "UDP" {
			line := protoNode.Line
			v.addErr(&line, fmt.Sprintf("containers.ports.protocol has unsupported value '%s'", s))
		}
	}
}

// ----- Probe -----

func validateProbe(v *vCtx, n *yaml.Node, prefix string) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, fmt.Sprintf("%s must be object", prefix))
		return
	}
	// httpGet (required) — HTTPGetAction
	hgNode, ok := mv.fields["httpGet"]
	if !ok {
		v.addErr(nil, fmt.Sprintf("%s.httpGet is required", prefix))
		return
	}
	validateHTTPGet(v, hgNode, prefix+".httpGet")
}

func validateHTTPGet(v *vCtx, n *yaml.Node, prefix string) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, fmt.Sprintf("%s must be object", prefix))
		return
	}

	// path (required, absolute)
	if pathNode, ok := mv.fields["path"]; !ok {
		v.addErr(nil, fmt.Sprintf("%s.path is required", prefix))
	} else if s, ok := getScalarString(pathNode); !ok {
		line := pathNode.Line
		v.addErr(&line, fmt.Sprintf("%s.path must be string", prefix))
	} else if !strings.HasPrefix(s, "/") {
		line := pathNode.Line
		v.addErr(&line, fmt.Sprintf("%s.path has invalid format '%s'", prefix, s))
	}

	// port (required, int 1..65535)
	if portNode, ok := mv.fields["port"]; !ok {
		v.addErr(nil, fmt.Sprintf("%s.port is required", prefix))
	} else if s, ok := getScalarString(portNode); !ok {
		line := portNode.Line
		v.addErr(&line, fmt.Sprintf("%s.port must be int", prefix))
	} else {
		val, err := strconv.Atoi(s)
		if err != nil {
			line := portNode.Line
			v.addErr(&line, fmt.Sprintf("%s.port must be int", prefix))
		} else if !portInRange(val) {
			line := portNode.Line
			v.addErr(&line, fmt.Sprintf("%s.port value out of range", prefix))
		}
	}
}

// ----- ResourceRequirements -----

func validateResources(v *vCtx, n *yaml.Node) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, "containers.resources must be object")
		return
	}
	// requests (optional), limits (optional)
	if reqNode, ok := mv.fields["requests"]; ok {
		validateResourceSet(v, reqNode, "containers.resources.requests")
	}
	if limNode, ok := mv.fields["limits"]; ok {
		validateResourceSet(v, limNode, "containers.resources.limits")
	}
}

func validateResourceSet(v *vCtx, n *yaml.Node, prefix string) {
	mv, ok := getMapping(n)
	if !ok {
		line := n.Line
		v.addErr(&line, fmt.Sprintf("%s must be object", prefix))
		return
	}
	for key, val := range mv.fields {
		switch key {
		case "cpu":
			// cpu — integer
			if s, ok := getScalarString(val); !ok {
				line := val.Line
				v.addErr(&line, fmt.Sprintf("%s.cpu must be int", prefix))
			} else if _, err := strconv.Atoi(s); err != nil {
				line := val.Line
				v.addErr(&line, fmt.Sprintf("%s.cpu must be int", prefix))
			}
		case "memory":
			// memory — string в Gi|Mi|Ki
			if s, ok := getScalarString(val); !ok {
				line := val.Line
				v.addErr(&line, fmt.Sprintf("%s.memory must be string", prefix))
			} else if !reMem.MatchString(s) {
				line := val.Line
				v.addErr(&line, fmt.Sprintf("%s.memory has invalid format '%s'", prefix, s))
			}
		default:
			// неизвестный ресурс разрешаем (или можно ругаться — задание не требует)
		}
	}
}

// ----- main -----

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s <path-to-yaml>\n", os.Args[0])
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	filename := flag.Arg(0)
	data, err := os.ReadFile(filename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: cannot read file content: %v\n", filename, err)
		os.Exit(1)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		// Unmarshal сам ставит Line/Column для места ошибки,
		// но в сообщении достаточно общей формулировки
		fmt.Fprintf(os.Stderr, "%s: cannot unmarshal file content: %v\n", filename, err)
		os.Exit(1)
	}

	v := &vCtx{filename: filename}
	validateTop(v, &root)

	if v.hasErrs() {
		v.flush()
		os.Exit(1)
	}
	// всё ок
	os.Exit(0)
}
