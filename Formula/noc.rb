# Written by packaging/homebrew-formula.sh when v0.1.4 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.1.4"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.4/noc-darwin-arm64"
      sha256 "edeef63065b8d7308218141bb33995010bb5b76caaf6943ed1cdd2a00a4095af"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.4/noc-darwin-amd64"
      sha256 "bfdf7c155a4f3c3a42afb96a252c7e5589c9579d3039f294a76133952550892c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.4/noc-linux-arm64"
      sha256 "39ecde5b84d5db697c44924fe015adc7ac42ca9701328b2cf7a0628d335216c6"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.4/noc-linux-amd64"
      sha256 "564623eef1411e54610be568c203ad0adf458b4d999534b5a79eeb8b575ad54f"
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
        https://github.com/ahparikh/noc/releases/download/v0.1.4/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
