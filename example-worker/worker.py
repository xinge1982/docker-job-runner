"""Demo adapter only: replace the candidate logic with an existing program."""
import argparse
import json
import sys
from pathlib import Path


def main():
    if sys.argv[1:] == ["--jobrunner-schema"]:
        print(json.dumps({
            "version": 1,
            "parameters": [],
            "environment_variables": [],
        }))
        return
    parser = argparse.ArgumentParser()
    parser.add_argument("--stage", choices=("preview", "apply"), required=True)
    parser.add_argument("--input", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    payload = json.loads(Path(args.input).read_text(encoding="utf-8"))
    output_dir = Path(args.output)

    if args.stage == "preview":
        candidates = payload.get("candidates", [])
        if not isinstance(candidates, list) or any(not isinstance(c, dict) or not isinstance(c.get("id"), str) for c in candidates):
            raise ValueError("candidates must be a list of objects with string id")
        if len({c["id"] for c in candidates}) != len(candidates):
            raise ValueError("candidate ids must be unique")
        result = {"stage": "preview", "source_version": payload.get("source_version"),
                  "candidates": candidates, "count": len(candidates)}
    else:
        preview = json.loads(Path("/job/preview/result.json").read_text(encoding="utf-8"))
        if payload.get("source_version") != preview.get("source_version"):
            raise ValueError("source_version differs from preview")
        approved = payload.get("candidate_ids")
        if not isinstance(approved, list) or any(not isinstance(x, str) for x in approved) or len(set(approved)) != len(approved):
            raise ValueError("candidate_ids must be a unique list of strings")
        by_id = {c["id"]: c for c in preview["candidates"]}
        unknown = set(approved) - set(by_id)
        if unknown:
            raise ValueError(f"unknown candidate ids: {sorted(unknown)}")
        # Demo only: no database mutation. The real adapter calls the existing
        # program here and records each committed object ID and item result.
        result = {"stage": "apply", "source_version": preview["source_version"],
                  "items": [{"id": candidate_id, "status": "demo_only"} for candidate_id in approved],
                  "count": len(approved)}

    (output_dir / "result.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"event": "progress", "stage": args.stage, "done": result["count"],
                      "total": result["count"]}, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
