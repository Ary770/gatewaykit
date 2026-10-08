#!/usr/bin/env python3
"""Build and run the browser demo; --check runs functional checks and exits."""
import argparse
import signal
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

DEMO_DIRECTORY = Path(__file__).resolve().parent
REPOSITORY_ROOT = DEMO_DIRECTORY.parent
DEMO_URL = "http://127.0.0.1:8080/demo/index.html"
REQUIRED_PORTS = (8080, 3099, *range(3001, 3007))


def require_available_ports():
    for port in REQUIRED_PORTS:
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                listener.bind(("127.0.0.1", port))
            except OSError as error:
                raise RuntimeError(f"Port {port} is already in use; stop that service first.") from error


def ensure_processes_running(processes):
    for process in processes:
        if process.poll() is not None:
            raise RuntimeError(f"Demo process exited ({process.returncode}): {process.args[0]}")


def wait_until_ready(processes):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        ensure_processes_running(processes)
        try:
            for port in REQUIRED_PORTS:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    pass
            return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError("Demo services did not become ready within ten seconds.")


def stop_processes(processes):
    for process in reversed(processes):
        if process.poll() is None:
            process.terminate()
    for process in processes:
        try:
            process.wait(timeout=7)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def run_demo(check):
    require_available_ports()
    with tempfile.TemporaryDirectory(prefix="gatewaykit-demo-") as build_directory:
        gateway = str(Path(build_directory) / "gatewaykit")
        mock = str(Path(build_directory) / "mock")
        subprocess.run(["go", "build", "-o", gateway, "."], cwd=REPOSITORY_ROOT, check=True)
        subprocess.run(["go", "build", "-o", mock, "./cmd/mock"], cwd=REPOSITORY_ROOT, check=True)
        commands = [
            [sys.executable, "-m", "http.server", "3099", "--bind", "127.0.0.1", "--directory", str(DEMO_DIRECTORY / "web")],
            [mock],
            [gateway, "-config", str(DEMO_DIRECTORY / "gateway.yaml")],
        ]
        processes = []
        try:
            for command in commands:
                processes.append(subprocess.Popen(command, cwd=REPOSITORY_ROOT))
            wait_until_ready(processes)
            print(f"Demo ready: {DEMO_URL}\nPress Ctrl+C to stop.", flush=True)
            if check:
                subprocess.run([sys.executable, str(DEMO_DIRECTORY / "functional_test.py")], check=True)
                return
            while True:
                ensure_processes_running(processes)
                time.sleep(0.5)
        finally:
            stop_processes(processes)


def interrupt_demo(signum, frame):
    raise KeyboardInterrupt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="Run live checks, then stop all demo services")
    arguments = parser.parse_args()
    signal.signal(signal.SIGTERM, interrupt_demo)
    try:
        run_demo(arguments.check)
    except KeyboardInterrupt:
        return 0
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"Demo failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
