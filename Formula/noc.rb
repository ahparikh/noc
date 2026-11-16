# Written by packaging/homebrew-formula.sh when v0.2.1 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.2.1"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.1/noc-darwin-arm64"
      sha256 "77921771fdae9771423947d90920e1e286fa11e2148727b7c3cc7a66ca403996"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.1/noc-darwin-amd64"
      sha256 "8b7a8ce23d04a1edcbe5b1bb937951f30e9e76a848d8d0b6a0e2063ecb395492"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.1/noc-linux-arm64"
      sha256 "ce2ea91acde4e75054eebdef51336fe022b2c1a59a5d642da6a1cb02fe531ca3"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.2.1/noc-linux-amd64"
      sha256 "2300e59b3bacb5eb2f890266f6edfd9503d2cd8aa3d792c34aadeddd3707886a"
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
        https://github.com/ahparikh/noc/releases/download/v0.2.1/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
