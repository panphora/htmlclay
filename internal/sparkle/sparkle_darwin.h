#ifndef HTMLCLAY_SPARKLE_DARWIN_H
#define HTMLCLAY_SPARKLE_DARWIN_H

char *hcSparkleLoad(const char *frameworkPath);
void hcSparkleStart(void);
void hcSparkleInstallNow(void);
char *hcSparkleInfoString(const char *key);

#endif
