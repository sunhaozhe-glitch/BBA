#!/usr/bin/env python3
"""Offline runner guards: no DB, Docker or network used by negative cases."""
import importlib.util,json,os,secrets,sys,tempfile,unittest
from pathlib import Path
from unittest import mock
spec=importlib.util.spec_from_file_location('runner',Path(__file__).with_name('b6-product.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
class Guards(unittest.TestCase):
 def setUp(self):
  self.temp=tempfile.TemporaryDirectory(dir=os.environ['TMPDIR']);self.addCleanup(self.temp.cleanup)
  self.root=Path(self.temp.name)/'project';self.results=self.root/'evals/results';self.results.mkdir(parents=True,mode=0o700)
  self.patch=mock.patch.object(m,'ROOT',self.root);self.patch.start();self.addCleanup(self.patch.stop)
 def test_path_matrix(self):
  allowed=self.results/'independent-name';self.assertEqual(m.checked_output(str(allowed)),allowed);self.assertFalse(allowed.exists())
  existing=self.results/'exists';existing.mkdir();outside=Path(self.temp.name)/'outside';outside.mkdir()
  link=self.results/'link';link.symlink_to(outside,target_is_directory=True)
  broken=self.results/'broken';broken.symlink_to(outside/'missing')
  unsafe=self.results/'unsafe';unsafe.mkdir();unsafe.chmod(0o777)
  regular=self.results/'file';regular.write_text('owned')
  cases=[Path('/'),self.root,self.root/'evals',self.results,outside/'new',Path(str(self.results)+'-suffix')/'new',self.results/'..'/'escape',link/'new',broken,existing,unsafe/'new',self.results/'missing'/'new',regular/'new']
  for p in cases:
   with self.subTest(path=str(p)),self.assertRaises((ValueError,OSError)):m.checked_output(str(p))
  with mock.patch.object(m.os,'getuid',return_value=os.getuid()+1),self.assertRaises(ValueError):m.checked_output(str(allowed))
  trusted=self.results/'group';trusted.mkdir();trusted.chmod(0o770)
  with mock.patch.object(m,'_private_group',return_value=False),self.assertRaises(ValueError):m.checked_output(str(trusted/'new'))
 def test_inherited_dsn_before_any_resource(self):
  for key in ['BBA_TASKENGINE_DSN','BBA_FIXTURE_DSN']:
   p=self.results/('dsn-'+key)
   with mock.patch.dict(os.environ,{key:'postgres://untrusted.invalid/not-used'}),mock.patch.object(sys,'argv',['runner',str(p)]),mock.patch.object(m.subprocess,'run') as run,mock.patch.object(m.socket,'socket') as sock,self.assertRaisesRegex(ValueError,'DSN'):
    m.main()
   run.assert_not_called();sock.assert_not_called();self.assertFalse(p.exists())
 def test_nonce_matrix(self):
  n='a'*64
  self.assertEqual(m.checked_nonce({'nonce':n},n),{'nonce':n})
  for value,expected in [({'nonce':'b'*64},n),({},n),(None,n),({'nonce':n},'short'),({'nonce':n},'g'*64)]:
   with self.subTest(value=value),self.assertRaises(ValueError):m.checked_nonce(value,expected)
 def test_wrong_startup_nonce_exact_cleanup(self):
  # Exercise main's finally path with strictly mocked resource transport.
  target=self.results/'wrong-startup'
  class Bridge:
   def bind(self,*a):pass
   def getsockname(self):return ('127.0.0.1',12345)
   def listen(self):pass
   def settimeout(self,*a):pass
   def close(self):pass
   def connect_ex(self,*a):return 111
   def __enter__(self):return self
   def __exit__(self,*a):pass
  class Thread:
   def __init__(self,*a,**kw):pass
   def start(self):pass
   def join(self,**kw):pass
  def run(cmd,**kw):
   inspect='inspect' in cmd
   result=mock.Mock(returncode=1 if inspect else 0,stdout='',stderr='No such resource' if inspect else '')
   result.check_returncode=lambda:None
   return result
  def start(*a,**kw):
   (target/'fixture-config.json').write_text(json.dumps({'nonce':'wrong','production_binary':True}))
   return mock.Mock(poll=lambda:0)
  with mock.patch.dict(os.environ,{'BBA_TASKENGINE_DSN':'','BBA_FIXTURE_DSN':''}),mock.patch.object(sys,'argv',['runner',str(target)]),mock.patch.object(m.socket,'socket',Bridge),mock.patch.object(m.threading,'Thread',Thread),mock.patch.object(m.subprocess,'run',side_effect=run) as calls,mock.patch.object(m.subprocess,'Popen',side_effect=start),mock.patch.object(Path,'read_text',autospec=True) as read:
   # Keep real reads other than the source kernel, which is never executed.
   def read_owned(p,*a,**kw):
    if p.name=='kernel.sh':return 'kernel stub'
    with p.open() as f:return f.read()
   read.side_effect=read_owned
   with self.assertRaisesRegex(ValueError,'nonce mismatch'):m.main()
  manifest=json.loads((target/'resources.json').read_text());cleanup=json.loads((target/'cleanup-readback.json').read_text())
  self.assertTrue(all(x['absent'] for x in cleanup['resources']));self.assertTrue(cleanup['loopback_closed'])
  removed=[c.args[0][-1] for c in calls.call_args_list if c.args[0][:3] in [['docker','rm','-f'],['docker','volume','rm']]]
  self.assertEqual(set(removed),set(manifest['containers']+manifest['volumes']))
if __name__=='__main__':unittest.main(verbosity=2)
