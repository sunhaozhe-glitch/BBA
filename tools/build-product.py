#!/usr/bin/env python3
"""Build dependency-free embedded product assets and lossless NapCat registry."""
from pathlib import Path
import argparse, collections, hashlib, json, re, shutil
ROOT=Path(__file__).resolve().parents[1]
SOURCE=ROOT/'frontend/napcat-initial.txt'
REF=Path('/home/sun/.hermes/profiles/astra/cache/scratch/artex-ui-v0.3.24-reference')
def parse(text):
    lines=[(i+1,s.strip()) for i,s in enumerate(text.splitlines()) if s.strip()]
    if len(lines)%3: raise ValueError('nonempty lines must be label/category/identifier triples')
    entries=[]
    candidates={'ocr_image':'.ocr_image','set_group_sign':'send_group_sign','ArkShareGroup':'send_group_ark_share','ArkSharePeer':'send_ark_share'}
    for i in range(0,len(lines),3):
        (line,label),(_,category),(_,identifier)=lines[i:i+3]
        if not re.fullmatch(r'[.]?[A-Za-z_][A-Za-z0-9_]*',identifier):raise ValueError(f'invalid identifier at line {line+2}: {identifier}')
        risk='unverified'
        if any(x in identifier.lower() for x in ['cookie','csrf','credential','clientkey','rkey']):risk='credential_blocked'
        if identifier in ['send_packet','.handle_quick_operation']:risk='raw_or_compound_blocked'
        entries.append(dict(id=identifier,name=label,category=category,source_line=line,schema=None,schema_status='unverified',support='not_verified',enabled=False,risk=risk,instance_support=None,call_history=None,alias_candidate=candidates.get(identifier),alias_status='candidate_unverified' if identifier in candidates else None))
    counts=collections.Counter(e['id'] for e in entries)
    return dict(source_sha256=hashlib.sha256(text.encode()).hexdigest(),entry_count=len(entries),unique_count=len(counts),duplicates={k:v for k,v in counts.items() if v>1},alias_candidates=[{'source':k,'target':v,'verified':False} for k,v in candidates.items() if k in counts and v in counts],entries=entries)
def main():
    ap=argparse.ArgumentParser();ap.add_argument('--source',type=Path,default=SOURCE);args=ap.parse_args()
    dest=ROOT/'backend/cmd/taskengine-server/product';dest.mkdir(exist_ok=True)
    data=parse(args.source.read_text());(dest/'tools.json').write_text(json.dumps(data,ensure_ascii=False,indent=2)+'\n')
    for name in ['index.html','app.css','app.js']:
        shutil.copyfile(ROOT/'frontend'/name,dest/name)
    print(json.dumps({k:v for k,v in data.items() if k!='entries'},ensure_ascii=False,indent=2))
if __name__=='__main__':main()
