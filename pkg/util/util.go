package util

import (
	"bytes"
	"fmt"
	"k8s.io/klog/v2"
	"math/rand"
	"strings"

	"github.com/caoyingjunz/pixiulib/exec"
	"github.com/caoyingjunz/pixiulib/strutil"
)

func InSlice(s string, ss []string) bool {
	for _, sl := range ss {
		if sl == s {
			return true
		}
	}

	return false
}

func TrimAndFilter(input []string) []string {
	result := make([]string, 0, len(input))
	seen := make(map[string]struct{})

	for _, s := range input {
		trimmed := strings.TrimSpace(s) // 去除前后空格
		if trimmed == "" {              // 过滤空字符串或纯空格
			continue
		}
		if _, exists := seen[trimmed]; !exists { // 检查是否已存在
			result = append(result, trimmed)
			seen[trimmed] = struct{}{} // 标记为已处理
		}
	}

	return result
}

func KeyFunc(key interface{}) (int64, int64, error) {
	str, ok := key.(string)
	if !ok {
		return 0, 0, fmt.Errorf("failed to convert %v to string", key)
	}
	parts := strings.Split(str, "/")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("parts length not 2")
	}

	objectId, err := strutil.ParseInt64(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to Parse taskId to Int64 %v", err)
	}
	resourceVersion, err := strutil.ParseInt64(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("failed to Parse resourceVersion to Int64 %v", err)
	}

	return objectId, resourceVersion, nil
}

func GenRandInt(min, max int) int {
	return rand.Intn(max-min+1) + min
}

func ToRegexp(pattern string) string {
	var buffer bytes.Buffer
	buffer.WriteString("^")
	for _, ch := range pattern {
		switch ch {
		case '*':
			buffer.WriteString(".*")
		case '.', '+', '?', '|', '(', ')', '[', ']', '{', '}', '^', '$', '\\':
			buffer.WriteString("\\")
			buffer.WriteRune(ch)
		default:
			buffer.WriteRune(ch)
		}
	}
	return buffer.String()
}

func RunCmd(exec exec.Interface, cmd []string) ([]byte, error) {
	klog.Infof("%s is running", cmd)
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		klog.Errorf("failed to run %v %v %v", cmd, string(out), err)
		return nil, fmt.Errorf("failed to run %v %v %v", cmd, string(out), err)
	}

	return out, nil
}
