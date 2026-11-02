"""Bounded, secret-safe evidence. Never persist raw RPC payloads."""
import hashlib
import json
import re

LIMIT = 8192
SECRET = re.compile(r'(?i)(password|passwd|secret|token|api[_-]?key|authorization|credential|cookie)')
TEXT = re.compile(r'''(?i)(\b(?:password|passwd|secret|token|api[_-]?key|authorization|credential|cookie)\b[\s"']*[:=]\s*)(?:"[^"\n]*"|'[^'\n]*'|(?:Bearer\s+)?[^\s,;"'}]+)''')
BEARER = re.compile(r'(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*')
URL_AUTH = re.compile(r'(?i)(https?://)[^\s/@:]+:[^\s/@]+@')
KEY_TEXT = re.compile(r'\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[A-Z0-9]{16})\b')


def redact(value):
    if isinstance(value, dict):
        return {str(k): '[REDACTED]' if SECRET.search(str(k)) else redact(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [redact(v) for v in value[:100]]
    if isinstance(value, str):
        safe = KEY_TEXT.sub('[REDACTED]', URL_AUTH.sub(r'\1[REDACTED]@', value))
        return BEARER.sub('Bearer [REDACTED]', TEXT.sub(r'\1[REDACTED]', safe))[:2048]
    return value


def encoded(value):
    text = json.dumps(redact(value), ensure_ascii=True, separators=(',', ':'))
    if len(text.encode()) > LIMIT:
        return json.dumps({'truncated': True, 'redacted_sha256': hashlib.sha256(text.encode()).hexdigest()})
    return text


def summary(value):
    safe = encoded(value)
    return {'redacted_sha256': hashlib.sha256(safe.encode()).hexdigest(), 'bytes': len(safe.encode())}
