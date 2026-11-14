# Written by packaging/homebrew-formula.sh when v0.1.1 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.1.1"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.1/noc-darwin-arm64"
      sha256 "ddf2577c3c943f95ea29f643d5ca67fda135e9584e61c04b4178fa4e502138b0"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.1/noc-darwin-amd64"
      sha256 "6be2e8851ac5efca4ca2fa1b2dd8d2c5945e2bc6d850514acc1fe5b0fb2aa511"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.1/noc-linux-arm64"
      sha256 "076512cbe366da12578fb293eeb41706628d8669e43c1e420810d646b0d95b69"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.1/noc-linux-amd64"
      sha256 "fd173a1885f5dd97713505340ed9c7e639082133b39212ee3fe1875fb781ee44"
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
        https://github.com/ahparikh/noc/releases/download/v0.1.1/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
