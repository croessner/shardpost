# COMPRESS=DEFLATE round trip in perl (Compress::Raw::Zlib is core).
perl -e '
use IO::Socket::INET; use Compress::Raw::Zlib;
my $s = IO::Socket::INET->new(PeerAddr => "shardpost:143") or die;
sub line { my $l = <$s>; $l =~ s/\r?\n//; $l }
print "S: ", substr(line(), 0, 40), "\n";
print $s "a1 LOGIN test test\r\n"; print "S: ", substr(line(), 0, 30), "\n";
print $s "a2 COMPRESS DEFLATE\r\n"; print "S: ", line(), "\n";
my ($d, $st) = Compress::Raw::Zlib::Deflate->new(-WindowBits => -15, -AppendOutput => 1);
my ($i, $st2) = Compress::Raw::Zlib::Inflate->new(-WindowBits => -15, -AppendOutput => 1);
sub send_c { my $out = ""; $d->deflate($_[0], $out); $d->flush($out, Z_SYNC_FLUSH); print $s $out; }
sub recv_c { my $tag = shift; my $buf = ""; while ($buf !~ /^$tag /m) { my $raw; sysread($s, $raw, 65536) or die "eof"; $i->inflate($raw, $buf); } $buf }
send_c("a3 SELECT INBOX\r\n"); my $r = recv_c("a3"); print "S: $_\n" for grep { /EXISTS|^a3/ } split /\r\n/, $r;
send_c("a4 FETCH 1 (UID FLAGS)\r\n"); $r = recv_c("a4"); print "S: $_\n" for split /\r\n/, $r;
send_c("a5 LOGOUT\r\n"); $r = recv_c("a5"); print "S: ", (split /\r\n/, $r)[-1], "\n";
'
