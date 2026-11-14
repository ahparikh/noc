# Written by packaging/homebrew-formula.sh when v0.1.0 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.1.0"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.0/noc-darwin-arm64"
      sha256 "4b026a45f4ab0f63dbc94e15f3b5be4929f5e007635661e7fafb770a45b4e75d"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.0/noc-darwin-amd64"
      sha256 "6529c68cd74485ae0088e60e93a493094d968f4a037f65270541fcb5348a5eaa"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.0/noc-linux-arm64"
      sha256 "5222200c239ff295dfb6e2e1b68898534962d4bff0643ab090dd1fe5f14a6ccc"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.0/noc-linux-amd64"
      sha256 "351c91fb1911d6cfe1ad251e45b99ea02101c927879836cc523c0e9c6f5f52e7"
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
        https://github.com/ahparikh/noc/releases/download/v0.1.0/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
