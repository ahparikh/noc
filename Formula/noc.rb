# Written by packaging/homebrew-formula.sh when v0.4.0 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.4.0"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.4.0/noc-darwin-arm64"
      sha256 "07a3b35ba57d2ecd4f51db45b637489bfb3f07ff9bae81c910053e8cefd9388a"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.4.0/noc-darwin-amd64"
      sha256 "ce176d557ad38450e5a2d2cf8fad387147362ed04abedd3283b6f86d6c4c3901"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.4.0/noc-linux-arm64"
      sha256 "a4cc5747de02dd8399ff174aca8a85b774577714f6643eb9e93a32e9e7d80164"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.4.0/noc-linux-amd64"
      sha256 "1010821059984a476f6daee192e9bf722e51178b8e80ddf7dfb4a528230a6fb6"
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
        https://github.com/ahparikh/noc/releases/download/v0.4.0/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
