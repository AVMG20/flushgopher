#!/usr/bin/env python3
"""Report cache operations per phase of a storm run: count and time span.
usage: storm_report.py <out-dir>..."""
import re, sys, collections

line_re = re.compile(r'^([\d.]+) \[(\d+) [^\]]*\] (.*)$')
arg_re = re.compile(r'"((?:[^"\\]|\\.)*)"')

for d in sys.argv[1:]:
    phases = collections.OrderedDict()
    cur, mark_t = 'startup', None
    for line in open(d + '/monitor.txt', errors='replace'):
        m = line_re.match(line.strip())
        if not m: continue
        t, db, args = float(m.group(1)), m.group(2), arg_re.findall(m.group(3))
        if not args: continue
        cmd = args[0].upper()
        if cmd == 'ECHO' and args[1].startswith('MARK '):
            cur, mark_t = args[1][5:], t
            phases[cur] = {'ops': collections.Counter(), 'first': None, 'last': None, 'mark': t, 'cleans': 0}
            continue
        if cur not in phases: continue
        p = phases[cur]
        op = None
        if cmd == 'SMEMBERS': op = 'tag ' + re.sub(r'^[0-9a-f]{3}_', '', args[1][6:])
        elif cmd == 'FLUSHDB': op = f'FLUSHDB db{db}'
        elif cmd == 'DEL' and any(a.startswith('zc:k:') for a in args[1:]): op = 'DEL ids'
        if op:
            p['ops'][op] += 1
            p['first'] = p['first'] or t
            p['last'] = t
    print(f'== {d}')
    for name, p in phases.items():
        if name == 'end': continue
        total = sum(p['ops'].values())
        span = f"{p['last'] - p['mark']:.1f}s after the checkout" if p['last'] else 'no activity'
        print(f"  {name}: {total} clean ops, last one {span}")
        for op, n in sorted(p['ops'].items(), key=lambda x: -x[1]):
            print(f'      {n:4d} × {op}')
