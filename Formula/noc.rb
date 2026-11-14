# Written by packaging/homebrew-formula.sh when v0.1.3 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.1.3"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.3/noc-darwin-arm64"
      sha256 "314857a4bc1964a3f623d97525d83d8e3b5396e0200cc4764a164f23c56ee3fc"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.3/noc-darwin-amd64"
      sha256 "40957a2dad33dc8d105497d675129ee9794fa9f188b4d9b84f35656f436aff4c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.3/noc-linux-arm64"
      sha256 "b6fa85532f2ebb26100b34ce45c71ee1d6daadb6ed62185a0faa9344f8dfcda4"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.3/noc-linux-amd64"
      sha256 "ceaa5119cc86c72167b076b319193a3057743921388d9616a658192f814c1051"
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
        https://github.com/ahparikh/noc/releases/download/v0.1.3/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
