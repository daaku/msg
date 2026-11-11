importpath=github.com/daaku/msg
pkgname=$(basename "$importpath")
pkgver=$(git rev-list --count HEAD)
pkgrel=1
pkgdesc='self hosted group chat with passkeys, push notifications and S3 backed media'
arch=('x86_64' 'aarch64')
url="https://$importpath"
depends=('imagemagick' 'ffmpeg')
optdepends=('libheif: HEIC and AVIF images')
makedepends=('go')
backup=('etc/conf.d/msg')

build() {
  cd ..
  CGO_ENABLED=0 go build -trimpath -o msg .
}

package() {
  cd ..
  install -Dm755 msg "$pkgdir/usr/bin/msg"
  install -Dm644 systemd/msg.service "$pkgdir/usr/lib/systemd/system/msg.service"
  install -Dm644 systemd/msg.socket "$pkgdir/usr/lib/systemd/system/msg.socket"
  install -Dm600 systemd/msg.conf "$pkgdir/etc/conf.d/msg"
}
