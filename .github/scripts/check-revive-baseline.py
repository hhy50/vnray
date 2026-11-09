import collections
import json
import pathlib
import sys


def findings(path):
    data = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
    result = collections.Counter()
    for item in data:
        filename = item["Position"]["Start"]["Filename"].replace(chr(92), "/")
        if item["RuleName"] == "package-comments":
            filename = str(pathlib.PurePosixPath(filename).parent)
        result[(filename, item["RuleName"], item["Failure"])] += 1
    return result


current = findings(sys.argv[1])
baseline = collections.Counter({
    (item["file"], item["rule"], item["failure"]): item["count"]
    for item in json.loads(pathlib.Path(sys.argv[2]).read_text(encoding="utf-8"))
})
added = current - baseline
for (filename, rule, failure), count in sorted(added.items()):
    print(f"{filename}: {rule}: {failure} ({count} new)")
if added:
    sys.exit(1)
print(f"revive: no new findings ({sum(current.values())} existing)")
