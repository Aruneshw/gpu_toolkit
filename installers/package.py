"""Build standalone Windows ZIP and Linux tarballs from make dist output."""
import hashlib
import pathlib
import sys
import tarfile
import zipfile

root = pathlib.Path(__file__).resolve().parent.parent
dist = pathlib.Path(sys.argv[1]).resolve()
with zipfile.ZipFile(dist / 'meshcompute-windows-amd64.zip', 'w', zipfile.ZIP_DEFLATED) as archive:
    for name in ('meshcompute-windows-amd64.exe', 'SHA256SUMS'):
        archive.write(dist / name, name)
    for name in ('install.cmd', 'join.cmd', 'installers/install-windows.ps1',
                 'installers/install-prebuilt.ps1', 'installers/uninstall.ps1',
                 'docs/windows.md', 'docs/host-first-design.md', 'LICENSE'):
        archive.write(root / name, name)
for arch in ('amd64', 'arm64'):
    with tarfile.open(dist / f'meshcompute-linux-{arch}.tar.gz', 'w:gz') as archive:
        archive.add(dist / f'meshcompute-linux-{arch}', arcname='meshcompute')
        archive.add(root / 'docs/host-first-design.md', arcname='QUICKSTART.md')
        archive.add(root / 'LICENSE', arcname='LICENSE')
names = ['meshcompute-linux-amd64', 'meshcompute-linux-arm64', 'meshcompute-windows-amd64.exe',
         'meshcompute-windows-amd64.zip', 'meshcompute-linux-amd64.tar.gz', 'meshcompute-linux-arm64.tar.gz']
(dist / 'SHA256SUMS').write_text(''.join(
    f'{hashlib.sha256((dist / name).read_bytes()).hexdigest()}  {name}\n' for name in names))
