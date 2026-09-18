#!/usr/bin/env python3
"""Summarise per-step cache operations from redis MONITOR logs and diff tools.
usage: analyze.py <out-dir-a> <out-dir-b>"""
import re, sys, collections

line_re = re.compile(r'^[\d.]+ \[(\d+) [^\]]*\] (.*)$')
arg_re = re.compile(r'"((?:[^"\\]|\\.)*)"')

def strip(k):  # remove the 3 char id prefix + "_"
    return k[4:] if re.match(r'^[0-9a-f]{3}_', k) else k

def parse(path):
    steps = collections.OrderedDict(); cur = 'startup'; steps[cur] = collections.Counter()
    for line in open(path, errors='replace'):
        m = line_re.match(line.strip())
        if not m: continue
        db, args = m.group(1), [a.encode().decode('unicode_escape') for a in arg_re.findall(m.group(2))]
        if not args: continue
        cmd = args[0].upper()
        if cmd == 'ECHO' and args[1].startswith('MARK '):
            cur = args[1][5:]; steps.setdefault(cur, collections.Counter()); continue
        c = steps[cur]
        if cmd == 'SMEMBERS':
            c[f'tag {strip(args[1][6:])} (db{db})'] += 1
        elif cmd == 'FLUSHDB':
            c[f'FLUSHDB db{db}'] += 1
        elif cmd == 'DEL':
            for k in args[1:]:
                if k.startswith('zc:k:'): c[f'id {strip(k[5:])}'] += 1
        elif cmd == 'EVAL' or cmd == 'EVALSHA':
            c['lua'] += 1
    return steps

def fmt(c):
    return ', '.join(f'{k}' + (f' ×{n}' if n > 1 else '') for k, n in sorted(c.items())) or '—'

a, b = sys.argv[1], sys.argv[2]
A, B = parse(a + '/monitor.txt'), parse(b + '/monitor.txt')
agree = differ = 0
for step in list(dict.fromkeys(list(A) + list(B))):
    if step == 'end': continue
    ca, cb = A.get(step, collections.Counter()), B.get(step, collections.Counter())
    same = set(ca) == set(cb)
    agree += same; differ += not same
    print(('  SAME ' if same else '≠ DIFF ') + step)
    if not same or '-v' in sys.argv:
        print('       orig: ' + fmt(ca)); print('       go  : ' + fmt(cb))
    elif ca != cb:
        print('       (same ops, counts differ) orig: ' + fmt(ca) + ' | go: ' + fmt(cb))
print(f'\n{agree} steps identical, {differ} differ')
for name, d in (('orig', a), ('go', b)):
    print(f'{name}: removed generated files: ' + ', '.join(l.strip() for l in open(d + '/generated-removed.txt')) )
