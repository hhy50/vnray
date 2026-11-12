import subprocess
import sys


module = "github.com/hhy50/vnray/"
allowed = {
    "internal/pipe": set(),
    "internal/extension": set(),
    "internal/config": set(),
    "internal/stats": set(),
    "internal/monitor": {
        "internal/logging", "internal/metrics", "internal/pipe", "internal/routine",
        "internal/stats", "internal/transport",
    },
    "internal/dispatch": {
        "internal/logging", "internal/pipe", "internal/protocol", "internal/routine",
    },
    "internal/transport": {
        "internal/concurrency", "internal/dispatch", "internal/logging",
        "internal/protocol", "internal/routine", "internal/stats", "internal/tcp",
    },
    "internal/ratelimit": {"internal/concurrency", "internal/stats"},
    "internal/store": {"internal/logging", "internal/protocol"},
    "internal/plugin": {
        "internal/config", "internal/routine", "internal/store",
        "internal/tcp", "internal/traffic",
    },
}
forbidden = {
    "admin": (
        "gorm.io/", "database/sql", module + "internal/core",
        module + "internal/inbound", module + "internal/sqliteweb",
    ),
}
result = subprocess.run(
    [
        "go", "list", "-buildvcs=false",
        "-f", "{{.ImportPath}}{{range .Imports}} {{.}}{{end}}",
        *("./" + package for package in [*allowed, *forbidden]),
    ],
    check=True,
    capture_output=True,
    text=True,
)
errors = []
for line in result.stdout.splitlines():
    package, *imports = line.split()
    package = package.removeprefix(module)
    for dependency in imports:
        if any(
            dependency == blocked or dependency.startswith(blocked + "/")
            or (blocked.endswith("/") and dependency.startswith(blocked))
            for blocked in forbidden.get(package, ())
        ):
            errors.append(f"{package} must not import {dependency}")
        if package in allowed and dependency.startswith(module):
            target = dependency.removeprefix(module)
            if target not in allowed[package]:
                errors.append(f"{package} must not import {target}")
for error in errors:
    print(error)
sys.exit(bool(errors))
