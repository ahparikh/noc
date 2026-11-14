# Written by packaging/homebrew-formula.sh when v0.1.2 was released. Do not edit.
class Noc < Formula
  desc "Registry and control plane for coding-agent conversations"
  homepage "https://github.com/ahparikh/noc"
  version "0.1.2"

  on_macos do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.2/noc-darwin-arm64"
      sha256 "eff25d9b2e0e017dbaada584133a3e632c565da03dee1b75a610e729f84195fc"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.2/noc-darwin-amd64"
      sha256 "b7cff7f1a8fde8ceb2abe10ec7ef09ad95958856d851c0300504351531552589"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.2/noc-linux-arm64"
      sha256 "84520d4f3719ff2f67d0c059c562e918301f3838512c98202d8e968a851b6caa"
    end
    on_intel do
      url "https://github.com/ahparikh/noc/releases/download/v0.1.2/noc-linux-amd64"
      sha256 "9c9bd91b3c62762ecadfc739d8afbc8d58ab31e2ba1d6a1e87a86b3456398d71"
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
        https://github.com/ahparikh/noc/releases/download/v0.1.2/zellij-noc.wasm
      See https://github.com/ahparikh/noc#zellij-sidebar
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/noc version")
  end
end
