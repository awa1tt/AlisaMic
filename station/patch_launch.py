import os
import sys
import json
import time
from datetime import datetime
import paramiko

HOST = os.environ.get("STATION_HOST", "192.168.3.37")
PORT = int(os.environ.get("STATION_PORT", "2222"))
USER = os.environ.get("STATION_USER", "root")
PASS = os.environ.get("STATION_PASS", "1")
TARGET_HOST = os.environ.get("ALICE_TAP_TARGET", "192.168.3.5:5000")

LAUNCH_JSON_PATH = "/system/vendor/quasar/launch.json"
ORIGINAL_BAK_PATH = "/data/quasar/launch.json.bak-alisamic"
MICSTREAM_SO_PATH = "/data/quasar/micstream/libvqe_tap.so"

def run_cmd(client, cmd, timeout=15):
    stdin, stdout, stderr = client.exec_command(cmd, timeout=timeout)
    out = stdout.read().decode('utf-8', errors='replace')
    err = stderr.read().decode('utf-8', errors='replace')
    return stdout.channel.recv_exit_status(), out, err

def is_root_ro(client):
    code, out, _ = run_cmd(client, "mount | grep ' / '")
    return "ro," in out or "(ro" in out

def check_updatesd_safety(client):
    code, out, _ = run_cmd(client, "tail -n 25 /data/quasar/daemons_logs/updatesd.log 2>/dev/null")
    lower = out.lower()
    if "downloading" in lower or "applying update" in lower or "progress" in lower:
        print("[WARNING] Active OTA download detected in updatesd! Killing updatesd...")
        run_cmd(client, "pkill -9 -f 'maind --updatesd'")
        raise RuntimeError("updatesd was actively downloading an OTA update! Process killed.")
    print("[OK] updatesd is idle. No OTA downloads in progress.")

def patch_launch_json(host=HOST, port=PORT, user=USER, password=PASS, target_host=TARGET_HOST):
    print(f"[*] Connecting to {user}@{host}:{port} via SSH...")
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(host, port=port, username=user, password=password, timeout=10, look_for_keys=False, allow_agent=False)
    print("[+] Connected successfully!")

    try:
        # 1. Safety check updatesd
        check_updatesd_safety(client)

        # 2. Check original backup existence
        code, out, _ = run_cmd(client, f"[ -f '{ORIGINAL_BAK_PATH}' ] && echo 'EXISTS'")
        if "EXISTS" not in out:
            print(f"[*] Original backup not found at {ORIGINAL_BAK_PATH}. Creating pristine original backup...")
            run_cmd(client, f"cp {LAUNCH_JSON_PATH} {ORIGINAL_BAK_PATH}")
            print(f"[+] Pristine backup created: {ORIGINAL_BAK_PATH}")
        else:
            print(f"[+] Pristine backup preserved: {ORIGINAL_BAK_PATH}")

        # 3. Create timestamped backup
        ts = datetime.now().strftime("%Y%m%d_%H%M%S")
        dated_bak = f"/data/quasar/launch.json.bak-{ts}"
        run_cmd(client, f"cp {LAUNCH_JSON_PATH} {dated_bak}")
        print(f"[+] Timestamped backup created: {dated_bak}")

        # 4. Download current launch.json via cat
        print(f"[*] Reading {LAUNCH_JSON_PATH} from station...")
        stdin, stdout, stderr = client.exec_command(f"cat {LAUNCH_JSON_PATH}")
        raw_bytes = stdout.read()
        print(f"[*] Received {len(raw_bytes)} bytes.")

        # 5. Parse JSON safely on PC
        try:
            cfg = json.loads(raw_bytes.decode('utf-8'))
        except Exception as e:
            print(f"[-] Raw file is invalid JSON: {e}")
            print("[*] Attempting to fix known '\\r,' artifact...")
            clean_text = raw_bytes.replace(b",\r,\n", b",\n").replace(b",\r\n", b",\n").decode('utf-8', errors='replace')
            cfg = json.loads(clean_text)
            print("[+] Successfully parsed JSON after cleaning line endings!")

        # 6. Idempotently update envs for task with arg ["--platformd"]
        task_found = False
        target_env = f"ALICE_TAP_TARGET={target_host}"
        preload_env = f"LD_PRELOAD={MICSTREAM_SO_PATH}"

        for node in cfg.get("nodes", []):
            for task in node.get("tasks", []):
                args = task.get("arg", [])
                if "--platformd" in args:
                    task_found = True
                    envs = task.setdefault("envs", [])
                    # Filter out any existing/corrupted LD_PRELOAD or ALICE_TAP_TARGET
                    new_envs = []
                    for e in envs:
                        if e.startswith("LD_PRELOAD=") and "libvqe_tap" in e:
                            continue
                        if e.startswith("ALICE_TAP_TARGET="):
                            continue
                        new_envs.append(e)

                    # Append clean entries
                    new_envs.append(preload_env)
                    new_envs.append(target_env)
                    task["envs"] = new_envs
                    print(f"[+] Updated platformd envs: {new_envs}")
                    break
            if task_found:
                break

        if not task_found:
            raise RuntimeError("Task with arg '--platformd' not found in launch.json!")

        # 7. Serialize to formatted JSON with pure LF line endings
        serialized = json.dumps(cfg, indent=4, ensure_ascii=False) + "\n"
        # Validate that serialization is valid
        json.loads(serialized)
        serialized_bytes = serialized.encode('utf-8')
        print(f"[+] New JSON validated locally: {len(serialized_bytes)} bytes.")

        # 8. Remount / rw
        print("[*] Remounting / as rw...")
        code, out, err = run_cmd(client, "mount -o remount,rw /")
        if code != 0:
            raise RuntimeError(f"Failed to remount / rw: {err}")

        # 9. Upload new content via cat stdin
        print(f"[*] Writing updated configuration to {LAUNCH_JSON_PATH}...")
        stdin, stdout, stderr = client.exec_command(f"cat > {LAUNCH_JSON_PATH}")
        stdin.write(serialized_bytes)
        stdin.channel.shutdown_write()
        exit_code = stdout.channel.recv_exit_status()
        if exit_code != 0:
            err_msg = stderr.read().decode('utf-8')
            raise RuntimeError(f"Failed to write file: {err_msg}")

        # 10. Sync
        run_cmd(client, "sync")

        # 11. Remount / ro
        print("[*] Remounting / as ro...")
        run_cmd(client, "mount -o remount,ro /")
        run_cmd(client, "sync")

        if not is_root_ro(client):
            raise RuntimeError("CRITICAL: Root filesystem is still rw! Check mount status.")
        print("[+] Root filesystem verified ro.")

        # 12. Verification: Read back from station and validate JSON
        print("[*] Verifying file content directly from device...")
        stdin, stdout, stderr = client.exec_command(f"cat {LAUNCH_JSON_PATH}")
        verify_bytes = stdout.read()
        verify_cfg = json.loads(verify_bytes.decode('utf-8'))
        
        # Verify platformd envs in downloaded config
        v_envs = None
        for node in verify_cfg.get("nodes", []):
            for task in node.get("tasks", []):
                if "--platformd" in task.get("arg", []):
                    v_envs = task.get("envs", [])
                    break
        
        if preload_env not in v_envs or target_env not in v_envs:
            raise RuntimeError(f"Verification failed! Expected envs not found in verified config: {v_envs}")

        print(f"[+] Verification SUCCESS! {LAUNCH_JSON_PATH} on device is 100% valid JSON with AlisaMIC envs active.")
        return True

    finally:
        client.close()

if __name__ == "__main__":
    patch_launch_json()
