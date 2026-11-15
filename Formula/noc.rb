# Written by packaging/homebrew-formula.sh when v0.2.0 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.2.0"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.0/noc-darwin-arm64"
      sha256 "e905d2a3dfab376ea620b2927acf891d5adfab3804d2d6c8a16ac530177a96b4"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.0/noc-darwin-amd64"
      sha256 "cbf75939bc34ae6d47141e66a404d8c75c13e03f46907da9bc13a4f6c09572af"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.0/noc-linux-arm64"
      sha256 "826f414d44cd4497ce5f01149264006be65ad5776a51936f355d42f0f65ed2f7"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.0/noc-linux-amd64"
      sha256 "db8c87614d1c8179a692b1d8f972ea389de1c18c4e5983abd082911102c6852a"
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
        https://github.com/ahparikh/noc/releases/download/v0.2.0/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
