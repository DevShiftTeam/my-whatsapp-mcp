import io
import pathlib
import tarfile
import tempfile
import unittest
import verify_cloudflared as v

class VerificationTests(unittest.TestCase):
    def test_manifest_is_pinned_and_unique(self):
        m=v.load_manifest(pathlib.Path(__file__).resolve().parents[2]/"packaging/cloudflared-manifest.json")
        self.assertEqual(len(m["assets"]),3)
        for mutation in [None, [], dict(m,version="latest"),dict(m,assets=m["assets"]*2),dict(m,assets=[None]*3)]:
            with self.assertRaises(ValueError):v.validate_manifest(mutation)
    def test_safe_archive(self):
        def archive(name="cloudflared",kind=tarfile.REGTYPE,extra=False):
            out=io.BytesIO()
            with tarfile.open(fileobj=out,mode="w:gz") as tf:
                member=tarfile.TarInfo(name);member.size=3;member.type=kind
                tf.addfile(member,io.BytesIO(b"abc"))
                if extra:tf.addfile(tarfile.TarInfo("extra"))
            return out.getvalue()
        self.assertEqual(v.extract(archive(),3),b"abc")
        for data in [archive("../cloudflared"),archive(kind=tarfile.SYMTYPE),archive(extra=True)]:
            with self.assertRaises(ValueError):v.extract(data,3)
        with self.assertRaises(ValueError):v.extract(archive(),2)
    def test_architecture_and_size(self):
        with self.assertRaises(ValueError):v.architecture(b"wrong","linux/amd64")
        with self.assertRaises(ValueError):v.checked_bytes(b"abc",dict(archive_size=2,archive_sha256="0"*64),"archive")
    def test_symlink_and_writable_parent(self):
        # Use a canonical owned home subtree: shared /tmp is deliberately invalid.
        with tempfile.TemporaryDirectory(dir=pathlib.Path.home()) as tmp:
            root=pathlib.Path(tmp);p=root/"file";p.write_bytes(b"test");p.chmod(0o600)
            self.assertEqual(v.read_private(p,4),b"test")
            (root/"link").symlink_to(p)
            with self.assertRaises(ValueError):v.read_private(root/"link",4)
            root.chmod(0o777)
            with self.assertRaises(ValueError):v.read_private(p,4)
            root.chmod(0o700)
    def test_duplicate_and_oversize_json(self):
        with self.assertRaises(ValueError):v.unique_object([("a",1),("a",2)])

    def test_bundled_notices_cover_actual_modules(self):
        import json, hashlib
        root=pathlib.Path(__file__).resolve().parents[2]/"packaging/cloudflared-notices"
        manifest=json.loads((root/"modules.json").read_text())
        self.assertEqual(len(manifest["actual_modules"]),68)
        for module in manifest["actual_modules"]:
            self.assertTrue(module["notice_files"])
            for name in module["notice_files"]:
                data=(root/"modules"/(module["module"]+"@"+module["version"])/name).read_bytes()
                self.assertEqual(hashlib.sha256(data).hexdigest(),module["notice_sha256"][name])

    @unittest.skipUnless(v.platform.system()=="Darwin", "Darwin ACL policy")
    def test_acl_writer_refused(self):
        import subprocess
        with tempfile.TemporaryDirectory(dir=pathlib.Path.home()) as tmp:
            p=pathlib.Path(tmp)/"file";p.write_bytes(b"safe");p.chmod(0o600)
            subprocess.run(["/bin/chmod","+a","everyone allow write",str(p)],check=True)
            with self.assertRaises(ValueError):v.read_private(p,4)

    def test_read_bounds_and_wrong_hash(self):
        import hashlib
        with tempfile.TemporaryDirectory(dir=pathlib.Path.home()) as tmp:
            p=pathlib.Path(tmp)/"asset";p.write_bytes(b"abc")
            with self.assertRaises(ValueError):v.read_private(p,2)
        with self.assertRaises(ValueError):v.checked_bytes(b"abc",dict(archive_size=3,archive_sha256="0"*64),"archive")
        self.assertEqual(v.checked_bytes(b"abc",dict(archive_size=3,archive_sha256=hashlib.sha256(b"abc").hexdigest()),"archive"),b"abc")

if __name__=="__main__":unittest.main()
