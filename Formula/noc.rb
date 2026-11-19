# Written by packaging/homebrew-formula.sh when v0.3.0 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.3.0"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.3.0/noc-darwin-arm64"
      sha256 "58c0ca040097f376da0da7a1e7b52ec532e5623cfca6dc547b39deb257b0abed"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.3.0/noc-darwin-amd64"
      sha256 "83d8f728b793a99402b6942bb8c5aab25354e491998d18860ad81b635b25a87f"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.3.0/noc-linux-arm64"
      sha256 "9054989230cc34b0731fb0d5f34a8aad36db7498fe28e7f1fb6e2c45d3036413"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.3.0/noc-linux-amd64"
      sha256 "a8d53fd5c5730d842328d34b8041e53e9ff89fb3a946ea6ee8563621c0ab1b58"
    end
  end

  def install
    bin.install Dir["noc-*"].first => "noc"
  end

  def caveats
    <<~EOS
      Set noc up once:
        noc setup          # install the Claude Code and Codex hook handlers
        noc server --bg    # start the registry server
        noc doctor

      After an upgrade, restart the server:
        noc server restart

      The Zellij sidebar is set up once, in your own Zellij configuration,
      and loads its plugin by URL from the same release:
        https://github.com/ahparikh/noc/releases/download/v0.3.0/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
