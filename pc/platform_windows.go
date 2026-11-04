package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

const fwRuleName = "AlisaMicUDP"

func hidden(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return c
}

// openURL открывает ссылку в браузере по умолчанию.
func openURL(url string) error {
	return hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

// firewallRuleExists — есть ли в брандмауэре Windows разрешающее правило AlisaMic.
func firewallRuleExists() (bool, error) {
	err := hidden(exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+fwRuleName)).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil // netsh возвращает 1, если правил с таким именем нет
	}
	return false, err
}

func psEncode(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 0, len(u)*2)
	for _, r := range u {
		b = append(b, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// addFirewallRule добавляет входящее правило UDP для этого exe. Запрашивает права администратора (UAC).
func addFirewallRule() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	inner := fmt.Sprintf(`netsh --%% advfirewall firewall delete rule name=%[1]s
netsh --%% advfirewall firewall add rule name=%[1]s dir=in action=allow protocol=UDP program="%[2]s" enable=yes profile=any
exit $LASTEXITCODE`, fwRuleName, exe)
	outer := fmt.Sprintf(`try { $p = Start-Process powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList '-NoProfile','-EncodedCommand','%s'; exit $p.ExitCode } catch { exit 1223 }`, psEncode(inner))
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", psEncode(outer))).CombinedOutput()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1223 {
			return errors.New("запрос прав администратора отклонён")
		}
		return fmt.Errorf("не удалось добавить правило: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hideChild запускает дочерний процесс без консоли и окна: так ssh не пытается спросить пароль
// у терминала, а берёт его через SSH_ASKPASS.
func hideChild(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008} // DETACHED_PROCESS
}
