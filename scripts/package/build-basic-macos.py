#!/usr/bin/env python3
"""Create an unsigned BASIC candidate with an explicit optional helper selection.

No Go build, downloads, native execution, signing, or runtime qualification.
"""
import argparse
import hashlib
import json
import os
import pathlib
import shutil
import tempfile

import assemble_macos as assembler
import verify_cloudflared
import verify_macos as verifier


def hexadecimal(value, length):
    if not isinstance(value, str) or len(value) != length or any(c not in "0123456789abcdef" for c in value):
        assembler.fail()


def copy_input(source, destination, expected_sha256=None, executable=False):
    before = verify_cloudflared.qualified_path(source)
    if (before.st_uid != os.getuid() or before.st_nlink != 1 or before.st_size > 8 * 1024**3
            or (executable and not before.st_mode & 0o100)):
        assembler.fail()
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as incoming:
        opened = os.fstat(incoming.fileno())
        fields = ("st_dev", "st_ino", "st_mode", "st_nlink", "st_uid", "st_size", "st_mtime_ns", "st_ctime_ns")
        if any(getattr(before, field) != getattr(opened, field) for field in fields):
            assembler.fail()
        with destination.open("xb") as outgoing:
            digest = assembler.audited_digest(incoming, before.st_size, outgoing, macho=executable)
            outgoing.flush()
            os.fsync(outgoing.fileno())
        if expected_sha256 is not None and digest != expected_sha256:
            assembler.fail()
        after = source.lstat()
        if any(getattr(before, field) != getattr(after, field) for field in fields):
            assembler.fail()
    destination.chmod(0o500 if executable else 0o400)
    return before.st_size, digest


def build(application, application_sha256, source_revision, cloudflared_asset, output, cloudflared=None, without_cloudflared=False):
    hexadecimal(application_sha256, 64)
    hexadecimal(source_revision, 40)
    if type(without_cloudflared) is not bool or sum((cloudflared_asset is not None, cloudflared is not None, without_cloudflared)) != 1:
        assembler.fail()
    application, output = map(pathlib.Path, (application, output))
    inputs = [application, output]
    if not without_cloudflared:
        helper_input = pathlib.Path(cloudflared_asset if cloudflared is None else cloudflared)
        inputs.append(helper_input)
    if (any(not p.is_absolute() or str(p) != os.path.normpath(p) for p in inputs)
            or output.exists() or output.is_symlink() or output.suffix != ".app"
            or output.is_relative_to(assembler.REPOSITORY)):
        assembler.fail()
    verify_cloudflared.qualified_path(output.parent, leaf=False)
    assembler.private_directory(output.parent)
    artifacts = []
    with tempfile.TemporaryDirectory(prefix=".basic-package-", dir=output.parent) as temp:
        root = pathlib.Path(temp)

        def add(source, role, path, expected=None, executable=False):
            name = "input-" + str(len(artifacts))
            size, digest = copy_input(source, root / name, expected, executable)
            artifacts.append({"role": role, "input_path": name, "relative_path": path,
                              "size_bytes": size, "sha256": digest, "executable": executable, "macho": executable})

        add(application, "application", assembler.BASIC_PATHS["application"], application_sha256, True)
        if not without_cloudflared:
            pins = verify_cloudflared.load_manifest(assembler.REPOSITORY / "packaging/cloudflared-manifest.json")
            helper = root / "verified-cloudflared"
            pin = next(a for a in pins["assets"] if a["platform"] == "darwin/arm64")
            if cloudflared is None:
                verify_cloudflared.install(pins, "darwin/arm64", helper, asset_path=helper_input, version_probe=False)
                helper.chmod(0o500)
            else:
                size, unused = copy_input(helper_input, helper, pin["executable_sha256"], executable=True)
                if size != pin["executable_size"]:
                    assembler.fail()
            add(helper, "cloudflared", assembler.BASIC_PATHS["cloudflared"], pin["executable_sha256"], True)
        source = assembler.REPOSITORY
        add(source / "LICENSE", "license", assembler.BASIC_PATHS["license"])
        add(source / "THIRD_PARTY_NOTICES", "third_party_notices", "Contents/Resources/THIRD_PARTY_NOTICES")
        add(source / "docs/assets/logo.png", "logo", assembler.BASIC_PATHS["logo"])
        if not without_cloudflared:
            add(source / "packaging/cloudflared-manifest.json", "cloudflared_manifest", assembler.BASIC_PATHS["cloudflared_manifest"])
            notices = source / "packaging/cloudflared-notices"
            for notice in sorted(notices.rglob("*")):
                if notice.is_symlink():
                    assembler.fail()
                if not notice.is_dir():
                    add(notice, "third_party_notices", "Contents/Resources/notices/cloudflared/" + str(notice.relative_to(notices)))
        manifest = {"schema_version": 1, "kind": "unsigned_local_basic_candidate", "bundle_profile": "basic",
                    "platform": "darwin-arm64", "source_revision": source_revision,
                    "bundle_ready": True, "release_ready": False, "runtime_qualification": "NOT_RUN",
                    "ui": "embedded_in_application", "selected_encoder_profile": None, "ai_profiles": [],
                    "remote_helper": "none" if without_cloudflared else "pinned",
                    "required_roles": sorted(assembler.BASIC_CORE_ROLES if without_cloudflared else assembler.BASIC_ROLES),
                    "artifacts": artifacts}
        if not without_cloudflared:
            manifest["remote_helper_version"] = verify_cloudflared.VERSION
        manifest_path = root / "basic-candidate.json"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
        manifest_path.chmod(0o600)
        assembler.assemble(str(manifest_path), str(root), str(output))
        try:
            verifier.verify(str(output))
        except Exception:
            # Assembly exclusively created this output; existing outputs never reach here.
            shutil.rmtree(output)
            raise
    return output


def main():
    parser = argparse.ArgumentParser(description=__doc__, allow_abbrev=False)
    parser.add_argument("--application", type=pathlib.Path, required=True)
    parser.add_argument("--application-sha256", required=True)
    parser.add_argument("--source-revision", required=True, help="full lowercase source commit (40 hex characters)")
    helper = parser.add_mutually_exclusive_group(required=True)
    helper.add_argument("--cloudflared-asset", type=pathlib.Path, help="local archive matching packaging/cloudflared-manifest.json")
    helper.add_argument("--cloudflared", type=pathlib.Path, help="already extracted executable matching the same manifest's executable size and SHA-256")
    helper.add_argument("--without-cloudflared", action="store_true", help="explicit local-only candidate without the remote tunnel helper")
    parser.add_argument("--output", type=pathlib.Path, required=True, help="new .app in an existing private directory outside the checkout")
    args = parser.parse_args()
    try:
        result = build(args.application, args.application_sha256, args.source_revision, args.cloudflared_asset, args.output,
                       cloudflared=args.cloudflared, without_cloudflared=args.without_cloudflared)
    except Exception:
        raise SystemExit("Basic packaging refused: audited application, explicit helper choice and new private output required.") from None
    digest = hashlib.sha256((result / "Contents/candidate-manifest.json").read_bytes()).hexdigest()
    print("Basic candidate statically audited; unsigned; release_ready=false; runtime_qualification=NOT_RUN.")
    print("candidate_manifest_sha256=" + digest)


if __name__ == "__main__":
    main()
