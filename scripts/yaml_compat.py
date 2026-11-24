"""Minimal YAML loader/dumper with PyYAML preference and stdlib fallback."""

from __future__ import annotations

from typing import Any

try:
    import yaml as _pyyaml  # type: ignore
except ImportError:
    _pyyaml = None


def _parse_scalar(raw: str) -> Any:
    val = raw.strip()
    if val in {"true", "True"}:
        return True
    if val in {"false", "False"}:
        return False
    if val in {"null", "~", ""}:
        return None
    if (val.startswith('"') and val.endswith('"')) or (val.startswith("'") and val.endswith("'")):
        return val[1:-1]
    if val.isdigit():
        return int(val)
    return val


def _strip_comment(line: str) -> str:
    in_single = False
    in_double = False
    for idx, ch in enumerate(line):
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
        elif ch == "#" and not in_single and not in_double and (idx == 0 or line[idx - 1].isspace()):
            return line[:idx]
    return line


def _fallback_safe_load(text: str) -> Any:
    tokens: list[tuple[int, str]] = []
    for raw_line in text.splitlines():
        cleaned = _strip_comment(raw_line).rstrip()
        if not cleaned.strip():
            continue
        indent = len(cleaned) - len(cleaned.lstrip(" "))
        tokens.append((indent, cleaned.strip()))

    if not tokens:
        return None

    def parse_block(idx: int, min_indent: int) -> tuple[Any, int]:
        indent, content = tokens[idx]
        if content.startswith("- "):
            items: list[Any] = []
            while idx < len(tokens):
                cur_indent, cur_content = tokens[idx]
                if cur_indent < min_indent or not cur_content.startswith("- "):
                    break
                if cur_indent != indent:
                    break
                rest = cur_content[2:].strip()
                if rest.startswith("- "):
                    # Nested list item on same line: `- - a`
                    nested, idx = parse_nested_list_item(idx, cur_indent + 2, rest)
                    items.append(nested)
                elif ":" in rest and not rest.startswith(('"', "'")):
                    k, v = rest.split(":", 1)
                    obj: dict[str, Any] = {}
                    v_s = v.strip()
                    if v_s:
                        obj[k.strip()] = _parse_scalar(v_s)
                        idx += 1
                    else:
                        idx += 1
                        if idx < len(tokens) and tokens[idx][0] > cur_indent:
                            obj[k.strip()], idx = parse_block(idx, cur_indent + 1)
                        else:
                            obj[k.strip()] = None
                    if idx < len(tokens) and tokens[idx][0] > cur_indent:
                        extra, idx = parse_block(idx, cur_indent + 1)
                        if isinstance(extra, dict):
                            obj.update(extra)
                    items.append(obj)
                elif rest:
                    items.append(_parse_scalar(rest))
                    idx += 1
                else:
                    idx += 1
                    if idx < len(tokens) and tokens[idx][0] > cur_indent:
                        child, idx = parse_block(idx, cur_indent + 1)
                        items.append(child)
                    else:
                        items.append(None)
            return items, idx

        mapping: dict[str, Any] = {}
        while idx < len(tokens):
            cur_indent, cur_content = tokens[idx]
            if cur_indent < min_indent or cur_content.startswith("- "):
                break
            if cur_indent != indent:
                break
            if ":" not in cur_content:
                raise ValueError(f"Invalid YAML line: {cur_content}")
            k, v = cur_content.split(":", 1)
            key = k.strip().strip('"').strip("'")
            val_str = v.strip()
            if val_str:
                mapping[key] = _parse_scalar(val_str)
                idx += 1
            else:
                idx += 1
                if idx < len(tokens) and tokens[idx][0] > cur_indent:
                    mapping[key], idx = parse_block(idx, cur_indent + 1)
                else:
                    mapping[key] = None
        return mapping, idx

    def parse_nested_list_item(idx: int, sub_indent: int, rest: str) -> tuple[list[Any], int]:
        sub_items: list[Any] = [_parse_scalar(rest[2:].strip())]
        idx += 1
        while idx < len(tokens) and tokens[idx][0] >= sub_indent and tokens[idx][1].startswith("- "):
            sub_items.append(_parse_scalar(tokens[idx][1][2:].strip()))
            idx += 1
        return sub_items, idx

    parsed, _ = parse_block(0, tokens[0][0])
    return parsed


def _format_scalar(val: Any) -> str:
    if val is None:
        return "null"
    if isinstance(val, bool):
        return "true" if val else "false"
    if isinstance(val, (int, float)):
        return str(val)
    text = str(val)
    if text == "" or any(ch in text for ch in (":", "#", "{", "}", "[", "]", "\n")) or text.strip() != text:
        escaped = text.replace("\\", "\\\\").replace('"', '\\"')
        return f'"{escaped}"'
    return text


def _fallback_safe_dump(data: Any, indent: int = 0) -> str:
    pad = " " * indent
    if isinstance(data, dict):
        lines: list[str] = []
        for k, v in data.items():
            if isinstance(v, (dict, list)) and v:
                lines.append(f"{pad}{k}:")
                lines.append(_fallback_safe_dump(v, indent + 2).rstrip("\n"))
            elif isinstance(v, list):
                lines.append(f"{pad}{k}: []")
            elif isinstance(v, dict):
                lines.append(f"{pad}{k}: {{}}")
            else:
                lines.append(f"{pad}{k}: {_format_scalar(v)}")
        return "\n".join(lines) + "\n"
    if isinstance(data, list):
        lines = []
        for item in data:
            if isinstance(item, dict) and item:
                first = True
                for k, v in item.items():
                    prefix = f"{pad}- " if first else f"{pad}  "
                    first = False
                    if isinstance(v, (dict, list)) and v:
                        lines.append(f"{prefix}{k}:")
                        lines.append(_fallback_safe_dump(v, indent + 4).rstrip("\n"))
                    else:
                        lines.append(f"{prefix}{k}: {_format_scalar(v)}")
            elif isinstance(item, list):
                lines.append(f"{pad}-")
                lines.append(_fallback_safe_dump(item, indent + 2).rstrip("\n"))
            else:
                lines.append(f"{pad}- {_format_scalar(item)}")
        return "\n".join(lines) + "\n"
    return f"{pad}{_format_scalar(data)}\n"


def safe_load(text: str) -> Any:
    if _pyyaml is not None:
        return _pyyaml.safe_load(text)
    return _fallback_safe_load(text)


def safe_dump(data: Any, sort_keys: bool = False, allow_unicode: bool = True) -> str:
    if _pyyaml is not None:
        return _pyyaml.safe_dump(data, sort_keys=sort_keys, allow_unicode=allow_unicode)
    return _fallback_safe_dump(data)
